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
	"reflect"
	"testing"
)

func TestIsRAIDVolume(t *testing.T) {
	tests := []struct {
		name     string
		volumeID string
		want     bool
	}{
		{
			name:     "normal EBS volume ID",
			volumeID: "vol-0123456789abcdef0",
			want:     false,
		},
		{
			name:     "RAID0 composite volume ID",
			volumeID: "raid0:vol-aaa+vol-bbb",
			want:     true,
		},
		{
			name:     "RAID0 with three members",
			volumeID: "raid0:vol-aaa+vol-bbb+vol-ccc",
			want:     true,
		},
		{
			name:     "empty string",
			volumeID: "",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRAIDVolume(tt.volumeID)
			if got != tt.want {
				t.Errorf("IsRAIDVolume(%q) = %v, want %v", tt.volumeID, got, tt.want)
			}
		})
	}
}

func TestParseRAIDVolumeID(t *testing.T) {
	tests := []struct {
		name        string
		volumeID    string
		wantMode    string
		wantMembers []string
		wantErr     bool
	}{
		{
			name:        "valid two members",
			volumeID:    "raid0:vol-aaa+vol-bbb",
			wantMode:    "raid0",
			wantMembers: []string{"vol-aaa", "vol-bbb"},
			wantErr:     false,
		},
		{
			name:        "valid three members",
			volumeID:    "raid0:vol-aaa+vol-bbb+vol-ccc",
			wantMode:    "raid0",
			wantMembers: []string{"vol-aaa", "vol-bbb", "vol-ccc"},
			wantErr:     false,
		},
		{
			name:     "no colon separator",
			volumeID: "vol-0123456789abcdef0",
			wantErr:  true,
		},
		{
			name:     "empty mode",
			volumeID: ":vol-aaa+vol-bbb",
			wantErr:  true,
		},
		{
			name:     "only one member",
			volumeID: "raid0:vol-aaa",
			wantErr:  true,
		},
		{
			name:     "empty string",
			volumeID: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, members, err := ParseRAIDVolumeID(tt.volumeID)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRAIDVolumeID(%q) error = %v, wantErr %v", tt.volumeID, err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if mode != tt.wantMode {
				t.Errorf("ParseRAIDVolumeID(%q) mode = %q, want %q", tt.volumeID, mode, tt.wantMode)
			}
			if !reflect.DeepEqual(members, tt.wantMembers) {
				t.Errorf("ParseRAIDVolumeID(%q) members = %v, want %v", tt.volumeID, members, tt.wantMembers)
			}
		})
	}
}

func TestBuildAndParseRAIDVolumeIDRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		memberIDs []string
	}{
		{
			name:      "two members",
			mode:      "raid0",
			memberIDs: []string{"vol-aaa", "vol-bbb"},
		},
		{
			name:      "four members",
			mode:      "raid0",
			memberIDs: []string{"vol-111", "vol-222", "vol-333", "vol-444"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			volumeID := BuildRAIDVolumeID(tt.mode, tt.memberIDs)
			mode, members, err := ParseRAIDVolumeID(volumeID)
			if err != nil {
				t.Fatalf("round-trip failed: BuildRAIDVolumeID produced %q, ParseRAIDVolumeID returned error: %v", volumeID, err)
			}
			if mode != tt.mode {
				t.Errorf("round-trip mode = %q, want %q", mode, tt.mode)
			}
			if !reflect.DeepEqual(members, tt.memberIDs) {
				t.Errorf("round-trip members = %v, want %v", members, tt.memberIDs)
			}
		})
	}
}

func TestParseRaidParams(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		want    *RaidParams
		wantErr bool
	}{
		{
			name:    "no raid mode set",
			params:  map[string]string{"type": "gp3"},
			want:    nil,
			wantErr: false,
		},
		{
			name:    "empty raid mode",
			params:  map[string]string{RaidModeKey: ""},
			want:    nil,
			wantErr: false,
		},
		{
			name: "valid raid0 with default stripe size",
			params: map[string]string{
				RaidModeKey:    "raid0",
				MemberCountKey: "2",
			},
			want: &RaidParams{
				Mode:        "raid0",
				MemberCount: 2,
				StripeSize:  "256K",
			},
			wantErr: false,
		},
		{
			name: "valid raid0 with custom stripe size",
			params: map[string]string{
				RaidModeKey:    "raid0",
				MemberCountKey: "4",
				StripeSizeKey:  "512K",
			},
			want: &RaidParams{
				Mode:        "raid0",
				MemberCount: 4,
				StripeSize:  "512K",
			},
			wantErr: false,
		},
		{
			name: "unsupported raid mode",
			params: map[string]string{
				RaidModeKey:    "raid1",
				MemberCountKey: "2",
			},
			wantErr: true,
		},
		{
			name: "missing membercount",
			params: map[string]string{
				RaidModeKey: "raid0",
			},
			wantErr: true,
		},
		{
			name: "invalid membercount not a number",
			params: map[string]string{
				RaidModeKey:    "raid0",
				MemberCountKey: "abc",
			},
			wantErr: true,
		},
		{
			name: "membercount less than 2",
			params: map[string]string{
				RaidModeKey:    "raid0",
				MemberCountKey: "1",
			},
			wantErr: true,
		},
		{
			name: "camelCase keys from StorageClass",
			params: map[string]string{
				"raidMode":    "raid0",
				"memberCount": "2",
				"stripeSize":  "512K",
			},
			want: &RaidParams{
				Mode:        "raid0",
				MemberCount: 2,
				StripeSize:  "512K",
			},
			wantErr: false,
		},
		{
			name: "mixed case keys",
			params: map[string]string{
				"RaidMode":    "raid0",
				"MEMBERCOUNT": "3",
			},
			want: &RaidParams{
				Mode:        "raid0",
				MemberCount: 3,
				StripeSize:  "256K",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRaidParams(tt.params)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRaidParams() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseRaidParams() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGetRAIDDevicePaths(t *testing.T) {
	tests := []struct {
		name           string
		publishContext map[string]string
		want           []string
	}{
		{
			name:           "empty context",
			publishContext: map[string]string{},
			want:           []string{},
		},
		{
			name: "single device path",
			publishContext: map[string]string{
				"devicePath.0": "/dev/xvdba",
			},
			want: []string{"/dev/xvdba"},
		},
		{
			name: "multiple device paths ordered",
			publishContext: map[string]string{
				"devicePath.2": "/dev/xvdbc",
				"devicePath.0": "/dev/xvdba",
				"devicePath.1": "/dev/xvdbb",
			},
			want: []string{"/dev/xvdba", "/dev/xvdbb", "/dev/xvdbc"},
		},
		{
			name: "ignores non-devicePath keys",
			publishContext: map[string]string{
				"devicePath.0": "/dev/xvdba",
				"devicePath.1": "/dev/xvdbb",
				"raidMode":     "raid0",
				"stripeSize":   "256K",
			},
			want: []string{"/dev/xvdba", "/dev/xvdbb"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetRAIDDevicePaths(tt.publishContext)
			// Treat nil and empty slice as equivalent for the empty case
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetRAIDDevicePaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRAIDPublishContext(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		stripeSize  string
		devicePaths []string
		want        map[string]string
	}{
		{
			name:        "two devices",
			mode:        "raid0",
			stripeSize:  "256K",
			devicePaths: []string{"/dev/xvdba", "/dev/xvdbb"},
			want: map[string]string{
				"raidMode":     "raid0",
				"stripeSize":   "256K",
				"devicePath.0": "/dev/xvdba",
				"devicePath.1": "/dev/xvdbb",
			},
		},
		{
			name:        "three devices with custom stripe",
			mode:        "raid0",
			stripeSize:  "512K",
			devicePaths: []string{"/dev/xvdba", "/dev/xvdbb", "/dev/xvdbc"},
			want: map[string]string{
				"raidMode":     "raid0",
				"stripeSize":   "512K",
				"devicePath.0": "/dev/xvdba",
				"devicePath.1": "/dev/xvdbb",
				"devicePath.2": "/dev/xvdbc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRAIDPublishContext(tt.mode, tt.stripeSize, tt.devicePaths)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildRAIDPublishContext() = %v, want %v", got, tt.want)
			}
		})
	}
}
