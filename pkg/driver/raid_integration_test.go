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

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/cloud/metadata"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/driver/internal"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/mounter"
	"github.com/kubernetes-sigs/aws-ebs-csi-driver/pkg/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var stdVolCap = []*csi.VolumeCapability{
	{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	},
}

func TestRAIDCreateVolume(t *testing.T) {
	raidVolSize := int64(10 * 1024 * 1024 * 1024) // 10 GiB
	perMemberSize := util.RoundUpBytes(raidVolSize / 2)

	testCases := []struct {
		name      string
		req       *csi.CreateVolumeRequest
		setupMock func(mockCloud *cloud.MockCloud, ctx context.Context)
		expID     string
		expErr    bool
		errCode   codes.Code
	}{
		{
			name: "success raid0 with 2 members",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey:    "raid0",
					MemberCountKey: "2",
				},
			},
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq("raid-vol-member-0"), gomock.Any()).DoAndReturn(
					func(_ context.Context, name string, opts *cloud.DiskOptions) (*cloud.Disk, error) {
						if opts.CapacityBytes != perMemberSize {
							return nil, fmt.Errorf("expected member capacity %d, got %d", perMemberSize, opts.CapacityBytes)
						}
						return &cloud.Disk{
							VolumeID:         "vol-aaa",
							CapacityGiB:      util.BytesToGiB(perMemberSize),
							AvailabilityZone: "us-east-1a",
						}, nil
					})
				mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq("raid-vol-member-1"), gomock.Any()).Return(
					&cloud.Disk{
						VolumeID:         "vol-bbb",
						CapacityGiB:      util.BytesToGiB(perMemberSize),
						AvailabilityZone: "us-east-1a",
					}, nil)
			},
			expID: "raid0:vol-aaa+vol-bbb",
		},
		{
			name: "success raid0 with 4 members splits iops and throughput",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol-4",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey:    "raid0",
					MemberCountKey: "4",
					IopsKey:        "12000",
					ThroughputKey:  "1000",
				},
			},
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				for i := 0; i < 4; i++ {
					memberName := fmt.Sprintf("raid-vol-4-member-%d", i)
					volID := fmt.Sprintf("vol-%d", i)
					mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq(memberName), gomock.Any()).DoAndReturn(
						func(_ context.Context, _ string, opts *cloud.DiskOptions) (*cloud.Disk, error) {
							if opts.IOPS != 3000 {
								return nil, fmt.Errorf("expected IOPS 3000, got %d", opts.IOPS)
							}
							if opts.Throughput != 250 {
								return nil, fmt.Errorf("expected throughput 250, got %d", opts.Throughput)
							}
							return &cloud.Disk{
								VolumeID:         volID,
								CapacityGiB:      util.BytesToGiB(util.RoundUpBytes(raidVolSize / 4)),
								AvailabilityZone: "us-east-1a",
							}, nil
						})
				}
			},
			expID: "raid0:vol-0+vol-1+vol-2+vol-3",
		},
		{
			name: "rollback on member creation failure",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol-fail",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey:    "raid0",
					MemberCountKey: "3",
				},
			},
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				// First two succeed
				mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq("raid-vol-fail-member-0"), gomock.Any()).Return(
					&cloud.Disk{VolumeID: "vol-ok-0", CapacityGiB: 4, AvailabilityZone: "us-east-1a"}, nil)
				mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq("raid-vol-fail-member-1"), gomock.Any()).Return(
					&cloud.Disk{VolumeID: "vol-ok-1", CapacityGiB: 4, AvailabilityZone: "us-east-1a"}, nil)
				// Third fails
				mockCloud.EXPECT().CreateDisk(gomock.Eq(ctx), gomock.Eq("raid-vol-fail-member-2"), gomock.Any()).Return(
					nil, fmt.Errorf("EC2 error"))
				// Expect rollback deletes
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-ok-0")).Return(true, nil)
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-ok-1")).Return(true, nil)
			},
			expErr:  true,
			errCode: codes.Internal,
		},
		{
			name: "reject raid with snapshot source",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol-snap",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey:    "raid0",
					MemberCountKey: "2",
				},
				VolumeContentSource: &csi.VolumeContentSource{
					Type: &csi.VolumeContentSource_Snapshot{
						Snapshot: &csi.VolumeContentSource_SnapshotSource{
							SnapshotId: "snap-123",
						},
					},
				},
			},
			setupMock: nil,
			expErr:    true,
			errCode:   codes.InvalidArgument,
		},
		{
			name: "reject invalid raid mode",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol-bad-mode",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey:    "raid5",
					MemberCountKey: "3",
				},
			},
			setupMock: nil,
			expErr:    true,
			errCode:   codes.InvalidArgument,
		},
		{
			name: "reject missing membercount",
			req: &csi.CreateVolumeRequest{
				Name:               "raid-vol-no-count",
				CapacityRange:      &csi.CapacityRange{RequiredBytes: raidVolSize},
				VolumeCapabilities: stdVolCap,
				Parameters: map[string]string{
					RaidModeKey: "raid0",
				},
			},
			setupMock: nil,
			expErr:    true,
			errCode:   codes.InvalidArgument,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()

			mockCloud := cloud.NewMockCloud(mockCtl)
			if tc.setupMock != nil {
				tc.setupMock(mockCloud, ctx)
			}

			driver := ControllerService{
				cloud:    mockCloud,
				inFlight: internal.NewInFlight(),
				options:  &Options{},
			}

			resp, err := driver.CreateVolume(ctx, tc.req)
			if tc.expErr {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				if st, ok := status.FromError(err); ok && st.Code() != tc.errCode {
					t.Fatalf("Expected error code %v, got %v: %v", tc.errCode, st.Code(), err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if resp.GetVolume().GetVolumeId() != tc.expID {
				t.Fatalf("Expected volume ID %q, got %q", tc.expID, resp.GetVolume().GetVolumeId())
			}
		})
	}
}

func TestRAIDDeleteVolume(t *testing.T) {
	testCases := []struct {
		name      string
		volumeID  string
		setupMock func(mockCloud *cloud.MockCloud, ctx context.Context)
		expErr    bool
	}{
		{
			name:     "success delete all members",
			volumeID: "raid0:vol-aaa+vol-bbb",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa")).Return(true, nil)
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb")).Return(true, nil)
			},
		},
		{
			name:     "success when some members already deleted",
			volumeID: "raid0:vol-aaa+vol-bbb",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa")).Return(false, cloud.ErrNotFound)
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb")).Return(true, nil)
			},
		},
		{
			name:     "fail when member deletion errors",
			volumeID: "raid0:vol-aaa+vol-bbb",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa")).Return(true, nil)
				mockCloud.EXPECT().DeleteDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb")).Return(false, fmt.Errorf("EC2 error"))
			},
			expErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()

			mockCloud := cloud.NewMockCloud(mockCtl)
			if tc.setupMock != nil {
				tc.setupMock(mockCloud, ctx)
			}

			driver := ControllerService{
				cloud:    mockCloud,
				inFlight: internal.NewInFlight(),
				options:  &Options{},
			}

			_, err := driver.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: tc.volumeID})
			if tc.expErr && err == nil {
				t.Fatal("Expected error, got nil")
			}
			if !tc.expErr && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

func TestRAIDControllerPublishVolume(t *testing.T) {
	raidVolumeID := "raid0:vol-aaa+vol-bbb"
	nodeID := "i-123456789abcdef01"

	stdVolCapSingle := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}

	testCases := []struct {
		name      string
		setupMock func(mockCloud *cloud.MockCloud, ctx context.Context)
		expErr    bool
		expPaths  []string
	}{
		{
			name: "success attach all members",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().AttachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return("/dev/xvdba", nil)
				mockCloud.EXPECT().AttachDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb"), gomock.Eq(nodeID)).Return("/dev/xvdbb", nil)
			},
			expPaths: []string{"/dev/xvdba", "/dev/xvdbb"},
		},
		{
			name: "rollback on second member attach failure",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().AttachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return("/dev/xvdba", nil)
				mockCloud.EXPECT().AttachDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb"), gomock.Eq(nodeID)).Return("", fmt.Errorf("attach failed"))
				// Expect rollback
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return(nil)
			},
			expErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()

			mockCloud := cloud.NewMockCloud(mockCtl)
			if tc.setupMock != nil {
				tc.setupMock(mockCloud, ctx)
			}

			driver := ControllerService{
				cloud:    mockCloud,
				inFlight: internal.NewInFlight(),
				options:  &Options{},
			}

			resp, err := driver.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
				VolumeId:         raidVolumeID,
				NodeId:           nodeID,
				VolumeCapability: stdVolCapSingle,
				VolumeContext: map[string]string{
					StripeSizeContextKey: "256K",
				},
			})
			if tc.expErr {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			// Verify publish context contains device paths
			paths := GetRAIDDevicePaths(resp.GetPublishContext())
			if !reflect.DeepEqual(paths, tc.expPaths) {
				t.Fatalf("Expected device paths %v, got %v", tc.expPaths, paths)
			}
			if resp.GetPublishContext()[RaidModeContextKey] != "raid0" {
				t.Fatalf("Expected raidMode=raid0 in publish context")
			}
		})
	}
}

func TestRAIDControllerUnpublishVolume(t *testing.T) {
	raidVolumeID := "raid0:vol-aaa+vol-bbb"
	nodeID := "i-123456789abcdef01"

	testCases := []struct {
		name      string
		setupMock func(mockCloud *cloud.MockCloud, ctx context.Context)
		expErr    bool
	}{
		{
			name: "success detach all members",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return(nil)
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb"), gomock.Eq(nodeID)).Return(nil)
			},
		},
		{
			name: "success when member not found",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return(cloud.ErrNotFound)
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-bbb"), gomock.Eq(nodeID)).Return(nil)
			},
		},
		{
			name: "fail on non-NotFound error",
			setupMock: func(mockCloud *cloud.MockCloud, ctx context.Context) {
				mockCloud.EXPECT().DetachDisk(gomock.Eq(ctx), gomock.Eq("vol-aaa"), gomock.Eq(nodeID)).Return(fmt.Errorf("EC2 error"))
			},
			expErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mockCtl := gomock.NewController(t)
			defer mockCtl.Finish()

			mockCloud := cloud.NewMockCloud(mockCtl)
			if tc.setupMock != nil {
				tc.setupMock(mockCloud, ctx)
			}

			driver := ControllerService{
				cloud:    mockCloud,
				inFlight: internal.NewInFlight(),
				options:  &Options{},
			}

			_, err := driver.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
				VolumeId: raidVolumeID,
				NodeId:   nodeID,
			})
			if tc.expErr && err == nil {
				t.Fatal("Expected error, got nil")
			}
			if !tc.expErr && err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

func TestRAIDNodeStageVolume(t *testing.T) {
	raidVolumeID := "raid0:vol-aaa+vol-bbb"
	vgName := raidVGName(raidVolumeID)
	lvPath := fmt.Sprintf("/dev/%s/%s", vgName, raidLVName)

	testCases := []struct {
		name         string
		req          *csi.NodeStageVolumeRequest
		mounterMock  func(ctrl *gomock.Controller) *mounter.MockMounter
		metadataMock func(ctrl *gomock.Controller) *metadata.MockMetadataService
		expectedErr  error
	}{
		{
			name: "success create new raid",
			req: &csi.NodeStageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					RaidModeContextKey:   "raid0",
					StripeSizeContextKey: "256K",
					"devicePath.0":       "/dev/xvdba",
					"devicePath.1":       "/dev/xvdbb",
				},
			},
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().FindDevicePath("/dev/xvdba", "vol-aaa", "", "us-west-2").Return("/dev/nvme1n1", nil)
				m.EXPECT().FindDevicePath("/dev/xvdbb", "vol-bbb", "", "us-west-2").Return("/dev/nvme2n1", nil)
				m.EXPECT().LVPath(vgName, raidLVName).Return(lvPath)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("", 0, nil)
				m.EXPECT().IsVGActive(vgName).Return(false, nil)
				m.EXPECT().CreateStripedLV(vgName, raidLVName, 2, "256K", []string{"/dev/nvme1n1", "/dev/nvme2n1"}).Return(nil)
				m.EXPECT().PathExists("/staging/path").Return(false, nil)
				m.EXPECT().MakeDir("/staging/path").Return(nil)
				m.EXPECT().FormatAndMountSensitiveWithFormatOptions(lvPath, "/staging/path", "ext4", gomock.Any(), gomock.Nil(), gomock.Any()).Return(nil)
				return m
			},
			metadataMock: func(ctrl *gomock.Controller) *metadata.MockMetadataService {
				m := metadata.NewMockMetadataService(ctrl)
				m.EXPECT().GetRegion().Return("us-west-2").Times(2)
				return m
			},
			expectedErr: nil,
		},
		{
			name: "success reactivate existing vg",
			req: &csi.NodeStageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					RaidModeContextKey:   "raid0",
					StripeSizeContextKey: "256K",
					"devicePath.0":       "/dev/xvdba",
					"devicePath.1":       "/dev/xvdbb",
				},
			},
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().FindDevicePath("/dev/xvdba", "vol-aaa", "", "us-west-2").Return("/dev/nvme1n1", nil)
				m.EXPECT().FindDevicePath("/dev/xvdbb", "vol-bbb", "", "us-west-2").Return("/dev/nvme2n1", nil)
				m.EXPECT().LVPath(vgName, raidLVName).Return(lvPath)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("", 0, nil)
				m.EXPECT().IsVGActive(vgName).Return(true, nil)
				m.EXPECT().ActivateVG(vgName).Return(nil)
				m.EXPECT().PathExists("/staging/path").Return(true, nil)
				m.EXPECT().FormatAndMountSensitiveWithFormatOptions(lvPath, "/staging/path", "ext4", gomock.Any(), gomock.Nil(), gomock.Any()).Return(nil)
				return m
			},
			metadataMock: func(ctrl *gomock.Controller) *metadata.MockMetadataService {
				m := metadata.NewMockMetadataService(ctrl)
				m.EXPECT().GetRegion().Return("us-west-2").Times(2)
				return m
			},
			expectedErr: nil,
		},
		{
			name: "idempotent already staged",
			req: &csi.NodeStageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					RaidModeContextKey:   "raid0",
					StripeSizeContextKey: "256K",
					"devicePath.0":       "/dev/xvdba",
					"devicePath.1":       "/dev/xvdbb",
				},
			},
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().FindDevicePath("/dev/xvdba", "vol-aaa", "", "us-west-2").Return("/dev/nvme1n1", nil)
				m.EXPECT().FindDevicePath("/dev/xvdbb", "vol-bbb", "", "us-west-2").Return("/dev/nvme2n1", nil)
				m.EXPECT().LVPath(vgName, raidLVName).Return(lvPath)
				// Already mounted with the right device
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return(lvPath, 1, nil)
				return m
			},
			metadataMock: func(ctrl *gomock.Controller) *metadata.MockMetadataService {
				m := metadata.NewMockMetadataService(ctrl)
				m.EXPECT().GetRegion().Return("us-west-2").Times(2)
				return m
			},
			expectedErr: nil,
		},
		{
			name: "fail on device path not found",
			req: &csi.NodeStageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					RaidModeContextKey:   "raid0",
					StripeSizeContextKey: "256K",
					"devicePath.0":       "/dev/xvdba",
					"devicePath.1":       "/dev/xvdbb",
				},
			},
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().FindDevicePath("/dev/xvdba", "vol-aaa", "", "us-west-2").Return("", fmt.Errorf("device not found"))
				return m
			},
			metadataMock: func(ctrl *gomock.Controller) *metadata.MockMetadataService {
				m := metadata.NewMockMetadataService(ctrl)
				m.EXPECT().GetRegion().Return("us-west-2")
				return m
			},
			expectedErr: status.Errorf(codes.NotFound, "Failed to find RAID member device path /dev/xvdba: device not found"),
		},
		{
			name: "fail on too few device paths",
			req: &csi.NodeStageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType: "ext4",
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					},
				},
				PublishContext: map[string]string{
					RaidModeContextKey:   "raid0",
					StripeSizeContextKey: "256K",
					"devicePath.0":       "/dev/xvdba",
				},
			},
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				return mounter.NewMockMounter(ctrl)
			},
			metadataMock: func(ctrl *gomock.Controller) *metadata.MockMetadataService {
				return metadata.NewMockMetadataService(ctrl)
			},
			expectedErr: status.Errorf(codes.InvalidArgument, "RAID volume requires at least 2 device paths, got 1"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			var m *mounter.MockMounter
			if tc.mounterMock != nil {
				m = tc.mounterMock(ctrl)
			}
			var md *metadata.MockMetadataService
			if tc.metadataMock != nil {
				md = tc.metadataMock(ctrl)
			}

			driver := &NodeService{
				metadata:     md,
				mounter:      m,
				options:      &Options{},
				inFlight:     internal.NewInFlight(),
				raidStateDir: t.TempDir(),
			}

			_, err := driver.NodeStageVolume(t.Context(), tc.req)
			if tc.expectedErr != nil {
				if err == nil {
					t.Fatalf("Expected error %v, got nil", tc.expectedErr)
				}
				// Compare error codes
				expSt, _ := status.FromError(tc.expectedErr)
				gotSt, _ := status.FromError(err)
				if expSt.Code() != gotSt.Code() {
					t.Fatalf("Expected error code %v, got %v: %v", expSt.Code(), gotSt.Code(), err)
				}
				if !strings.Contains(gotSt.Message(), expSt.Message()) && expSt.Message() != gotSt.Message() {
					t.Fatalf("Expected error message containing %q, got %q", expSt.Message(), gotSt.Message())
				}
			} else if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

func TestRAIDNodeUnstageVolume(t *testing.T) {
	raidVolumeID := "raid0:vol-aaa+vol-bbb"
	vgName := raidVGName(raidVolumeID)
	memberDevices := []string{"/dev/nvme1n1", "/dev/nvme2n1"}

	// writeState creates a RAID state file in the given dir so loadRAIDState succeeds.
	writeState := func(t *testing.T, dir string) {
		t.Helper()
		state := raidState{
			CompositeVolumeID: raidVolumeID,
			VGName:            vgName,
			MemberDevices:     memberDevices,
		}
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("failed to marshal state: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, vgName+".json"), data, 0644); err != nil {
			t.Fatalf("failed to write state: %v", err)
		}
	}

	testCases := []struct {
		name        string
		setupState  bool // whether to pre-create the state file
		mounterMock func(ctrl *gomock.Controller) *mounter.MockMounter
		expectedErr error
	}{
		{
			name:       "success full teardown",
			setupState: true,
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("/dev/dm-0", 1, nil)
				m.EXPECT().Unstage("/staging/path").Return(nil)
				m.EXPECT().IsVGActive(vgName).Return(true, nil)
				m.EXPECT().RemoveVG(vgName).Return(nil)
				m.EXPECT().RemovePVs(memberDevices).Return(nil)
				return m
			},
			expectedErr: nil,
		},
		{
			name:       "success already unmounted",
			setupState: true,
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("", 0, nil)
				m.EXPECT().IsVGActive(vgName).Return(false, nil)
				m.EXPECT().RemovePVs(memberDevices).Return(nil)
				return m
			},
			expectedErr: nil,
		},
		{
			name:       "success without state file skips RemovePVs",
			setupState: false,
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("/dev/dm-0", 1, nil)
				m.EXPECT().Unstage("/staging/path").Return(nil)
				m.EXPECT().IsVGActive(vgName).Return(true, nil)
				m.EXPECT().RemoveVG(vgName).Return(nil)
				// No RemovePVs expectation — must NOT be called
				return m
			},
			expectedErr: nil,
		},
		{
			name:       "fail on vg removal error",
			setupState: false,
			mounterMock: func(ctrl *gomock.Controller) *mounter.MockMounter {
				m := mounter.NewMockMounter(ctrl)
				m.EXPECT().GetDeviceNameFromMount("/staging/path").Return("/dev/dm-0", 1, nil)
				m.EXPECT().Unstage("/staging/path").Return(nil)
				m.EXPECT().IsVGActive(vgName).Return(true, nil)
				m.EXPECT().RemoveVG(vgName).Return(fmt.Errorf("vg busy"))
				return m
			},
			expectedErr: status.Errorf(codes.Internal, "Failed to remove VG %s: vg busy", vgName),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			stateDir := t.TempDir()
			if tc.setupState {
				writeState(t, stateDir)
			}

			m := tc.mounterMock(ctrl)

			driver := &NodeService{
				mounter:      m,
				inFlight:     internal.NewInFlight(),
				options:      &Options{},
				raidStateDir: stateDir,
			}

			_, err := driver.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
				VolumeId:          raidVolumeID,
				StagingTargetPath: "/staging/path",
			})
			if tc.expectedErr != nil {
				if err == nil {
					t.Fatalf("Expected error, got nil")
				}
				expSt, _ := status.FromError(tc.expectedErr)
				gotSt, _ := status.FromError(err)
				if expSt.Code() != gotSt.Code() {
					t.Fatalf("Expected error code %v, got %v: %v", expSt.Code(), gotSt.Code(), err)
				}
			} else if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}
