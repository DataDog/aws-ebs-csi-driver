//go:build linux

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mounter

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/utils/exec"
)

// nsenterCommand wraps a command to run in the host's namespaces via nsenter.
// LVM uses POSIX semaphores and host-level /run/lock, so commands must execute
// in the host's PID/mount/network namespaces (like TopoLVM and OpenEBS do).
func nsenterCommand(e exec.Interface, cmd string, args ...string) exec.Cmd {
	nsenterArgs := append([]string{"-m", "-u", "-i", "-n", "-p", "-t", "1", cmd}, args...)
	return e.Command("nsenter", nsenterArgs...)
}

// CreateStripedLV creates physical volumes, a volume group, and a striped logical volume.
func (m *NodeMounter) CreateStripedLV(vgName string, lvName string, stripeCount int, stripeSize string, devices []string) error {
	for _, device := range devices {
		klog.V(4).InfoS("Creating physical volume", "device", device)
		output, err := nsenterCommand(m.Exec, "pvcreate", device).CombinedOutput()
		if err != nil {
			return fmt.Errorf("pvcreate failed for %s: output: %s, err: %w", device, string(output), err)
		}
	}

	klog.V(4).InfoS("Creating volume group", "vgName", vgName, "devices", devices)
	vgArgs := append([]string{vgName}, devices...)
	output, err := nsenterCommand(m.Exec, "vgcreate", vgArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vgcreate failed for %s: output: %s, err: %w", vgName, string(output), err)
	}

	klog.V(4).InfoS("Creating striped logical volume", "lvName", lvName, "vgName", vgName, "stripeCount", stripeCount, "stripeSize", stripeSize)
	output, err = nsenterCommand(m.Exec, "lvcreate",
		"--stripes", strconv.Itoa(stripeCount),
		"--stripesize", stripeSize,
		"-l", "100%VG",
		"-n", lvName,
		vgName,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("lvcreate failed for %s/%s: output: %s, err: %w", vgName, lvName, string(output), err)
	}

	return nil
}

// ActivateVG activates a volume group.
func (m *NodeMounter) ActivateVG(vgName string) error {
	klog.V(4).InfoS("Activating volume group", "vgName", vgName)
	output, err := nsenterCommand(m.Exec, "vgchange", "-ay", vgName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vgchange -ay failed for %s: output: %s, err: %w", vgName, string(output), err)
	}
	return nil
}

// DeactivateVG deactivates a volume group.
func (m *NodeMounter) DeactivateVG(vgName string) error {
	klog.V(4).InfoS("Deactivating volume group", "vgName", vgName)
	output, err := nsenterCommand(m.Exec, "vgchange", "-an", vgName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vgchange -an failed for %s: output: %s, err: %w", vgName, string(output), err)
	}
	return nil
}

// RemoveVG deactivates all logical volumes in the volume group and then removes it.
func (m *NodeMounter) RemoveVG(vgName string) error {
	klog.V(4).InfoS("Deactivating logical volumes in volume group", "vgName", vgName)
	output, err := nsenterCommand(m.Exec, "lvchange", "-an", vgName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("lvchange -an failed for %s: output: %s, err: %w", vgName, string(output), err)
	}

	klog.V(4).InfoS("Removing volume group", "vgName", vgName)
	output, err = nsenterCommand(m.Exec, "vgremove", "-f", vgName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("vgremove failed for %s: output: %s, err: %w", vgName, string(output), err)
	}
	return nil
}

// RemovePVs removes physical volumes for the given devices. It continues on error and collects all errors.
func (m *NodeMounter) RemovePVs(devices []string) error {
	var errs []string
	for _, device := range devices {
		klog.V(4).InfoS("Removing physical volume", "device", device)
		output, err := nsenterCommand(m.Exec, "pvremove", device).CombinedOutput()
		if err != nil {
			errs = append(errs, fmt.Sprintf("pvremove failed for %s: output: %s, err: %v", device, string(output), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// IsVGActive checks if a volume group exists and is active.
func (m *NodeMounter) IsVGActive(vgName string) (bool, error) {
	output, err := nsenterCommand(m.Exec, "vgs", "--noheadings", "-o", "vg_name", vgName).CombinedOutput()
	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "not found") {
			return false, nil
		}
		return false, fmt.Errorf("vgs failed for %s: output: %s, err: %w", vgName, outputStr, err)
	}
	return true, nil
}

// LVPath returns the device path for a logical volume.
func (m *NodeMounter) LVPath(vgName string, lvName string) string {
	return fmt.Sprintf("/dev/%s/%s", vgName, lvName)
}
