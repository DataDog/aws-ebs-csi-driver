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

package driver

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RAID StorageClass parameter keys (lowercase for case-insensitive matching).
const (
	RaidModeKey    = "raidmode"
	MemberCountKey = "membercount"
	StripeSizeKey  = "stripesize"
)

// Supported RAID modes.
const (
	RaidModeRAID0 = "raid0"
)

// Default values for RAID parameters.
const (
	DefaultStripeSize = "256K"
)

// PublishContext keys for RAID volumes.
const (
	RaidModeContextKey   = "raidMode"
	StripeSizeContextKey = "stripeSize"
	DevicePathPrefix     = "devicePath."
)

// RaidParams holds parsed RAID parameters from a StorageClass.
type RaidParams struct {
	Mode        string
	MemberCount int
	StripeSize  string
}

// IsRAIDVolume returns true if the given volume ID represents a RAID composite volume.
// RAID volume IDs contain a colon separator (e.g., "raid0:vol-aaa+vol-bbb"),
// whereas normal EBS volume IDs (e.g., "vol-0123456789abcdef0") do not.
func IsRAIDVolume(volumeID string) bool {
	return strings.Contains(volumeID, ":")
}

// ParseRAIDVolumeID splits a composite RAID volume ID into its mode and member IDs.
// The expected format is "mode:member1+member2+...memberN" with at least 2 members.
func ParseRAIDVolumeID(volumeID string) (mode string, members []string, err error) {
	parts := strings.SplitN(volumeID, ":", 2)
	if len(parts) != 2 {
		return "", nil, fmt.Errorf("invalid RAID volume ID %q: missing mode prefix", volumeID)
	}

	mode = parts[0]
	if mode == "" {
		return "", nil, fmt.Errorf("invalid RAID volume ID %q: empty mode", volumeID)
	}

	members = strings.Split(parts[1], "+")
	if len(members) < 2 {
		return "", nil, fmt.Errorf("invalid RAID volume ID %q: need at least 2 members, got %d", volumeID, len(members))
	}

	return mode, members, nil
}

// BuildRAIDVolumeID constructs a composite RAID volume ID from a mode and member IDs.
func BuildRAIDVolumeID(mode string, memberIDs []string) string {
	return mode + ":" + strings.Join(memberIDs, "+")
}

// ParseRaidParams extracts RAID parameters from a StorageClass parameter map.
// Returns nil if raidmode is not set. Returns an error if the parameters are invalid.
// Keys are matched case-insensitively since StorageClass parameters may use camelCase.
func ParseRaidParams(params map[string]string) (*RaidParams, error) {
	// Build a lowercase lookup map to handle camelCase keys from StorageClass
	lower := make(map[string]string, len(params))
	for k, v := range params {
		lower[strings.ToLower(k)] = v
	}

	mode, ok := lower[RaidModeKey]
	if !ok || mode == "" {
		return nil, nil
	}

	if mode != RaidModeRAID0 {
		return nil, fmt.Errorf("unsupported RAID mode %q: only %q is supported", mode, RaidModeRAID0)
	}

	countStr, ok := lower[MemberCountKey]
	if !ok || countStr == "" {
		return nil, fmt.Errorf("parameter %q is required when %q is set", MemberCountKey, RaidModeKey)
	}

	count, err := strconv.Atoi(countStr)
	if err != nil {
		return nil, fmt.Errorf("invalid %q value %q: %w", MemberCountKey, countStr, err)
	}
	if count < 2 {
		return nil, fmt.Errorf("invalid %q value %d: must be at least 2", MemberCountKey, count)
	}

	stripeSize := lower[StripeSizeKey]
	if stripeSize == "" {
		stripeSize = DefaultStripeSize
	}

	return &RaidParams{
		Mode:        mode,
		MemberCount: count,
		StripeSize:  stripeSize,
	}, nil
}

// GetRAIDDevicePaths extracts ordered device paths from a PublishContext map.
// It looks for keys matching "devicePath.N" and returns the values sorted by index.
func GetRAIDDevicePaths(publishContext map[string]string) []string {
	type indexedPath struct {
		index int
		path  string
	}

	var paths []indexedPath
	for k, v := range publishContext {
		if !strings.HasPrefix(k, DevicePathPrefix) {
			continue
		}
		idxStr := strings.TrimPrefix(k, DevicePathPrefix)
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		paths = append(paths, indexedPath{index: idx, path: v})
	}

	sort.Slice(paths, func(i, j int) bool {
		return paths[i].index < paths[j].index
	})

	result := make([]string, len(paths))
	for i, p := range paths {
		result[i] = p.path
	}
	return result
}

// BuildRAIDPublishContext creates a PublishContext map for a RAID volume.
func BuildRAIDPublishContext(mode string, stripeSize string, devicePaths []string) map[string]string {
	ctx := map[string]string{
		RaidModeContextKey:   mode,
		StripeSizeContextKey: stripeSize,
	}
	for i, path := range devicePaths {
		ctx[DevicePathPrefix+strconv.Itoa(i)] = path
	}
	return ctx
}
