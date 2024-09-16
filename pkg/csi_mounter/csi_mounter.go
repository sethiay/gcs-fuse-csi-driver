/*
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csimounter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sidecarmounter "github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/sidecar_mounter"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

const socketName = "socket"

// Mounter provides the Cloud Storage FUSE CSI implementation of mount.Interface
// for the linux platform.
type Mounter struct {
	mount.MounterForceUnmounter
	mux           sync.Mutex
	fuseSocketDir string
}

// New returns a mount.MounterForceUnmounter for the current system.
// It provides options to override the default mounter behavior.
// mounterPath allows using an alternative to `/bin/mount` for mounting.
func New(mounterPath, fuseSocketDir string) (mount.Interface, error) {
	m, ok := mount.New(mounterPath).(mount.MounterForceUnmounter)
	if !ok {
		return nil, errors.New("failed to cast mounter to MounterForceUnmounter")
	}

	return &Mounter{
		m,
		sync.Mutex{},
		fuseSocketDir,
	}, nil
}

func (m *Mounter) Mount(source string, target string, fstype string, options []string) error {
	m.mux.Lock()
	defer m.mux.Unlock()

	csiMountOptions, sidecarMountOptions := prepareMountOptions(options)

	// Prepare sidecar mounter MountConfig
	mc := sidecarmounter.MountConfig{
		BucketName: source,
		Options:    sidecarMountOptions,
	}
	msg, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("failed to marshal sidecar mounter MountConfig %v: %w", mc, err)
	}

	podID, volumeName, _ := util.ParsePodIDVolumeFromTargetpath(target)
	logPrefix := fmt.Sprintf("[Pod %v, Volume %v, Bucket %v]", podID, volumeName, source)

	klog.V(4).Infof("%v opening the device /dev/fuse", logPrefix)
	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open the device /dev/fuse: %w", err)
	}
	csiMountOptions = append(csiMountOptions, fmt.Sprintf("fd=%v", fd))

	klog.V(4).Infof("%v mounting the fuse filesystem", logPrefix)
	err = m.MountSensitiveWithoutSystemdWithMountFlags(source, target, fstype, csiMountOptions, nil, []string{"--internal-only"})
	if err != nil {
		return fmt.Errorf("failed to mount the fuse filesystem: %w", err)
	}

	go func() {
		// updateReadAhead may hang until the file descriptor (fd) is either consumed or canceled.
		// It will succeed if dfuse finishes the mount process, or it will fail if dfuse fails
		// or the mount point is cleaned up due to mounting failures.
		if err := updateReadAhead(target, 1024); err != nil {
			klog.Errorf("%v failed to update read_ahead: %v", logPrefix, err)
		} else {
			klog.Errorf("%v able to change read_ahead: %v", logPrefix, err)
		}

	}()

	listener, err := m.createSocket(target, logPrefix)
	if err != nil {
		// If mount failed at this step,
		// cleanup the mount point and allow the CSI driver NodePublishVolume to retry.
		klog.Warningf("%v failed to create socket, clean up the mount point", logPrefix)

		syscall.Close(fd)
		if m.UnmountWithForce(target, time.Second*5) != nil {
			klog.Warningf("%v failed to clean up the mount point", logPrefix)
		}

		return err
	}

	// Close the listener and fd after 1 hour timeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	go func() {
		<-ctx.Done()
		klog.V(4).Infof("%v closing the socket and fd", logPrefix)
		listener.Close()
		syscall.Close(fd)
	}()

	// Asynchronously waiting for the sidecar container to connect to the listener
	go startAcceptConn(listener, logPrefix, msg, fd, cancel)

	return nil
}

func (m *Mounter) UnmountWithForce(target string, umountTimeout time.Duration) error {
	m.cleanupSocket(target)

	return m.MounterForceUnmounter.UnmountWithForce(target, umountTimeout)
}

func (m *Mounter) Unmount(target string) error {
	m.cleanupSocket(target)

	return m.MounterForceUnmounter.Unmount(target)
}

func (m *Mounter) createSocket(target string, logPrefix string) (net.Listener, error) {
	klog.V(4).Infof("%v passing the descriptor", logPrefix)

	// Prepare the temp emptyDir path
	emptyDirBasePath, err := util.PrepareEmptyDir(target, true)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare emptyDir path: %w", err)
	}

	// Create socket base path.
	// Need to create symbolic link of emptyDirBasePath to socketBasePath,
	// because the socket absolute path is longer than 104 characters,
	// which will cause "bind: invalid argument" errors.
	socketBasePath := util.GetSocketBasePath(target, m.fuseSocketDir)
	if err := os.Symlink(emptyDirBasePath, socketBasePath); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("failed to create symbolic link to path %q: %w", socketBasePath, err)
	}

	klog.V(4).Infof("%v create a listener using the socket", logPrefix)
	l, err := net.Listen("unix", filepath.Join(socketBasePath, socketName))
	if err != nil {
		return nil, fmt.Errorf("failed to create a listener using the socket: %w", err)
	}

	// Change the socket ownership
	targetSocketPath := filepath.Join(emptyDirBasePath, socketName)
	if err = os.Chown(filepath.Dir(emptyDirBasePath), webhook.NobodyUID, webhook.NobodyGID); err != nil {
		return nil, fmt.Errorf("failed to change ownership on base of emptyDirBasePath: %w", err)
	}
	if err = os.Chown(emptyDirBasePath, webhook.NobodyUID, webhook.NobodyGID); err != nil {
		return nil, fmt.Errorf("failed to change ownership on emptyDirBasePath: %w", err)
	}
	if err = os.Chown(targetSocketPath, webhook.NobodyUID, webhook.NobodyGID); err != nil {
		return nil, fmt.Errorf("failed to change ownership on targetSocketPath: %w", err)
	}

	if _, err = os.Stat(targetSocketPath); err != nil {
		return nil, fmt.Errorf("failed to verify the targetSocketPath: %w", err)
	}

	return l, nil
}

func (m *Mounter) cleanupSocket(target string) {
	socketBasePath := util.GetSocketBasePath(target, m.fuseSocketDir)
	socketPath := filepath.Join(socketBasePath, socketName)
	if err := syscall.Unlink(socketPath); err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf("failed to clean up socket %q: %v", socketPath, err)
		}
	}

	if err := os.Remove(socketBasePath); err != nil {
		if !os.IsNotExist(err) {
			klog.Errorf("failed to clean up socket base path %q: %v", socketBasePath, err)
		}
	}
}

func startAcceptConn(l net.Listener, logPrefix string, msg []byte, fd int, cancel context.CancelFunc) {
	defer cancel()

	klog.V(4).Infof("%v start to accept connections to the listener.", logPrefix)
	a, err := l.Accept()
	if err != nil {
		klog.Errorf("%v failed to accept connections to the listener: %v", logPrefix, err)

		return
	}
	defer a.Close()

	klog.V(4).Infof("%v start to send file descriptor and mount options", logPrefix)
	if err = util.SendMsg(a, fd, msg); err != nil {
		klog.Errorf("%v failed to send file descriptor and mount options: %v", logPrefix, err)
	}

	klog.V(4).Infof("%v exiting the listener goroutine.", logPrefix)
}

func prepareMountOptions(options []string) ([]string, []string) {
	allowedOptions := map[string]bool{
		"exec":    true,
		"noexec":  true,
		"atime":   true,
		"noatime": true,
		"sync":    true,
		"async":   true,
		"dirsync": true,
	}

	csiMountOptions := []string{
		"nodev",
		"nosuid",
		"allow_other",
		"default_permissions",
		"rootmode=40000",
		fmt.Sprintf("user_id=%d", os.Getuid()),
		fmt.Sprintf("group_id=%d", os.Getgid()),
	}

	// users may pass options that should be used by Linux mount(8),
	// filter out these options and not pass to the sidecar mounter.
	validMountOptions := []string{"rw", "ro"}
	optionSet := sets.NewString(options...)
	for _, o := range validMountOptions {
		if optionSet.Has(o) {
			csiMountOptions = append(csiMountOptions, o)
			optionSet.Delete(o)
		}
	}

	for _, o := range optionSet.List() {
		if strings.HasPrefix(o, "o=") {
			v := o[2:]
			if allowedOptions[v] {
				csiMountOptions = append(csiMountOptions, v)
			} else {
				klog.Warningf("got invalid mount option %q. Will discard invalid options and continue to mount.", v)
			}
			optionSet.Delete(o)
		}
	}

	return csiMountOptions, optionSet.List()
}

func updateReadAhead(targetMountPath string, readAheadKB int64) error {
	cmd := exec.Command("mountpoint", "-d", targetMountPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		klog.Errorf("Error executing mountpoint command on target path %s: %v", targetMountPath, err)
		if exitError, ok := err.(*exec.ExitError); ok {
			fmt.Printf("Exit code: %d\n", exitError.ExitCode())
		}
		return err
	}

	outputStr := strings.TrimSpace(string(output))
	klog.Infof("output of mountpoint for target mount path %s: %s", targetMountPath, output)

	// Update the target value
	sysClassEchoCmd := fmt.Sprintf("echo %d > /sys/class/bdi/%s/read_ahead_kb", readAheadKB, outputStr)
	klog.V(2).Infof("Executing command %s", sysClassEchoCmd)
	cmd = exec.Command("sh", "-c", sysClassEchoCmd)
	_, err = cmd.CombinedOutput()
	if err != nil {
		return err
	}

	// Verify updated value
	sysClassCatCmd := fmt.Sprintf("cat /sys/class/bdi/%s/read_ahead_kb", outputStr)
	klog.V(2).Infof("Executing command %s", sysClassCatCmd)
	cmd = exec.Command("sh", "-c", sysClassCatCmd)
	op, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	klog.V(2).Infof("Output of %q : %s", sysClassCatCmd, op)

	opStr := strings.TrimSpace(string(op))
	updatedVal, err := strconv.ParseInt(opStr, 10, 0)
	if err != nil {
		return fmt.Errorf("invalid read_ahead_kb %v", err)
	}
	if updatedVal != readAheadKB {
		return fmt.Errorf("mismatch in read_ahead_kb, expected %d, got %d", readAheadKB, updatedVal)
	}

	return nil
}
