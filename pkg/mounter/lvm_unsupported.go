//go:build !linux

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
	"errors"
	"fmt"
	"strings"
)

func (m *NodeMounter) CreateStripedLV(vgName string, lvName string, stripeCount int, stripeSize string, devices []string) error {
	return errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) ActivateVG(vgName string) error {
	return errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) DeactivateVG(vgName string) error {
	return errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) RemoveVG(vgName string) error {
	return errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) RemovePVs(devices []string) error {
	return errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) IsVGActive(vgName string) (bool, error) {
	return false, errors.New("LVM operations are not supported on this platform")
}

func (m *NodeMounter) LVPath(vgName string, lvName string) string {
	escaped := strings.ReplaceAll(vgName, "-", "--") + "-" + strings.ReplaceAll(lvName, "-", "--")
	return fmt.Sprintf("/dev/mapper/%s", escaped)
}
