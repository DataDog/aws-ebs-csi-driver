# RAID Volume Mode - Design Doc

**Status**: Draft
**Author**: Antoine Gaillard
**Date**: 2026-03-06

## Problem Statement

Some workloads require storage throughput and IOPS beyond what a single EBS volume can provide. Today, users must manually provision multiple EBS volumes, attach them, and assemble a striped array — a process invisible to Kubernetes and incompatible with dynamic provisioning via PVCs.

This design adds a `raidMode` parameter to the EBS CSI driver, enabling transparent provisioning of multiple EBS volumes exposed as a single striped logical volume to the pod.

## Goals

- Dynamically provision N EBS volumes and expose them as a single striped device
- Fully transparent to the workload — one PVC, one mount
- Linear scaling of throughput and IOPS with member count
- Clean teardown on volume deletion
- No changes to the CSI spec or Kubernetes API

## Non-Goals

- RAID1/5/6/10 (no redundancy modes — EBS already replicates within AZ)
- Spanning multiple AZs
- Snapshot support for RAID volumes (future work)

## Why LVM over mdadm

- LVM is already kernel-native (device-mapper); `lvm2` userspace tools are lighter than `mdadm`
- `lvcreate --stripes N` gives the same striping semantics as mdadm RAID0
- LVM opens the door to **online resize** — extend PVs and grow the LV without teardown
- Cleaner device naming (`/dev/<vg>/<lv>`) — no `/dev/md/` namespace contention
- Future path to thin provisioning and LVM-level snapshots

## Architecture Overview

```
StorageClass (raidMode=raid0, memberCount=4)
        |
        v
  CreateVolume ──> creates N EBS volumes, returns composite volume ID
        |
        v
  ControllerPublishVolume ──> attaches all N volumes to node
        |
        v
  NodeStageVolume ──> discovers N devices, assembles LVM VG + striped LV, formats, mounts
        |
        v
  NodePublishVolume ──> bind mount (unchanged)
```

## Detailed Design

### 1. StorageClass Parameters

New parameters in `pkg/driver/constants.go`:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `raidMode` | string | no | RAID mode. Currently only `raid0` is supported |
| `memberCount` | int | yes (when raidMode set) | Number of EBS volumes to stripe across |
| `stripeSize` | string | no (default "256K") | LVM stripe unit size |

Example StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ebs-raid0-gp3
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "6000"
  throughput: "500"
  raidMode: "raid0"
  memberCount: "4"
  stripeSize: "256K"
volumeBindingMode: WaitForFirstConsumer
```

Each member volume gets `iops/N` and `throughput/N` — the aggregate matches the requested values.

### 2. Composite Volume ID

A RAID volume is identified by a composite ID encoding all member EBS volume IDs:

```
raid0:<vol-aaa>+<vol-bbb>+<vol-ccc>+<vol-ddd>
```

This ID is:
- Returned by `CreateVolume` as `VolumeId`
- Used in all subsequent CSI calls (`DeleteVolume`, `ControllerPublishVolume`, etc.)
- Parsed by helper functions to extract member IDs

```go
// pkg/driver/raid.go

const raidSeparator = "+"

func IsRAIDVolume(volumeID string) bool {
    return strings.Contains(volumeID, ":")  // has a mode prefix
}

func ParseRAIDVolumeID(volumeID string) (mode string, members []string, err error) {
    parts := strings.SplitN(volumeID, ":", 2)
    if len(parts) != 2 {
        return "", nil, fmt.Errorf("invalid raid volume ID: %s", volumeID)
    }
    mode = parts[0]
    members = strings.Split(parts[1], raidSeparator)
    if len(members) < 2 {
        return "", nil, fmt.Errorf("raid volume must have at least 2 members, got %d", len(members))
    }
    return mode, members, nil
}

func BuildRAIDVolumeID(mode string, memberIDs []string) string {
    return mode + ":" + strings.Join(memberIDs, raidSeparator)
}
```

### 3. Controller: CreateVolume

**File**: `pkg/driver/controller.go` — `CreateVolume()`

When `raidMode` is set in parameters:

1. Validate `raidMode` (only `raid0` supported), require `memberCount`
2. Compute per-member capacity: `requestedBytes / memberCount` (rounded up to GiB boundary)
3. Compute per-member IOPS/throughput: `total / memberCount`
4. Loop `memberCount` times, calling `d.cloud.CreateDisk()` for each member
   - Volume name: `{pvName}-member-{i}` (e.g., `pvc-abc-member-0`, `pvc-abc-member-1`)
   - Tag each member with `raid.ebs.csi.aws.com/array-id={pvName}` and `raid.ebs.csi.aws.com/member-index={i}`
5. On partial failure: delete all successfully created members, return error
6. Return composite volume ID and aggregate capacity

```go
// Pseudocode in CreateVolume
if raidMode != "" {
    memberIDs := make([]string, 0, memberCount)
    for i := 0; i < memberCount; i++ {
        memberOpts := *diskOptions
        memberOpts.CapacityBytes = perMemberBytes
        memberOpts.IOPS = diskOptions.IOPS / int32(memberCount)
        memberOpts.Throughput = diskOptions.Throughput / int32(memberCount)
        memberOpts.Tags["raid.ebs.csi.aws.com/array-id"] = volName
        memberOpts.Tags["raid.ebs.csi.aws.com/member-index"] = strconv.Itoa(i)

        disk, err := d.cloud.CreateDisk(ctx, fmt.Sprintf("%s-member-%d", volName, i), &memberOpts)
        if err != nil {
            for _, id := range memberIDs {
                d.cloud.DeleteDisk(ctx, id)
            }
            return nil, err
        }
        memberIDs = append(memberIDs, disk.VolumeID)
    }
    compositeID := BuildRAIDVolumeID(raidMode, memberIDs)
    // Return response with compositeID and total capacity
}
```

**Idempotency**: On retry, check if members with matching `raid.ebs.csi.aws.com/array-id` tag already exist via `GetDiskByName` for each member.

### 4. Controller: DeleteVolume

When `IsRAIDVolume(volumeID)`:

1. Parse member IDs from composite ID
2. Delete each member via `d.cloud.DeleteDisk()`
3. Continue on individual failures (idempotent — members may already be gone)
4. Return error only if any deletion fails with a non-NotFound error

### 5. Controller: ControllerPublishVolume (Attach)

When `IsRAIDVolume(volumeID)`:

1. Parse member IDs
2. Attach each member via `d.cloud.AttachDisk(ctx, memberID, nodeID)`
3. Collect all device paths
4. On partial failure: detach all successfully attached members, return error
5. Return all device paths in PublishContext:

```go
publishContext := map[string]string{
    "raidMode":    raidMode,
    "stripeSize":  stripeSize,  // from VolumeContext
}
for i, path := range devicePaths {
    publishContext[fmt.Sprintf("devicePath.%d", i)] = path
}
```

### 6. Controller: ControllerUnpublishVolume (Detach)

When `IsRAIDVolume(volumeID)`:

1. Parse member IDs
2. Detach each member via `d.cloud.DetachDisk(ctx, memberID, nodeID)`
3. Continue on individual failures (idempotent)

### 7. Node: NodeStageVolume — LVM Assembly

This is the critical path — where the striped logical volume is assembled.

**File**: `pkg/driver/node.go` — `NodeStageVolume()`

When `publishContext["raidMode"]` is set:

1. **Discover devices**: For each `devicePath.{i}` in PublishContext, resolve the actual device via `d.mounter.FindDevicePath()`
2. **Check idempotency**: If staging path is already mounted with the expected LV, return success
3. **Assemble LVM striped volume**:

```bash
# Create physical volumes
pvcreate /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1 /dev/nvme4n1

# Create volume group (name derived from composite volume ID)
vgcreate ebs-<id-hash> /dev/nvme1n1 /dev/nvme2n1 /dev/nvme3n1 /dev/nvme4n1

# Create striped logical volume using 100% of the VG
lvcreate --stripes 4 --stripesize 256K -l 100%VG -n data ebs-<id-hash>
```

4. **Format**: Run mkfs on `/dev/ebs-<id-hash>/data` with requested filesystem options
5. **Mount**: Mount the LV at the staging path

**VG/LV naming**: VG name is `ebs-<truncated-sha256-of-composite-id>`, LV name is always `data`. Deterministic naming ensures idempotency.

**Reassembly on reboot**: LVM metadata lives on the PVs. On reboot, `vgscan` + `vgchange -ay` reactivates the VG. NodeStageVolume handles this:

```go
// Try to activate existing VG first
err := exec("vgchange", "-ay", vgName)
if err != nil {
    // VG doesn't exist yet, create it
    exec("pvcreate", devices...)
    exec("vgcreate", vgName, devices...)
    exec("lvcreate", "--stripes", strconv.Itoa(len(devices)),
        "--stripesize", stripeSize, "-l", "100%VG", "-n", "data", vgName)
}
```

### 8. Node: NodeUnstageVolume (Teardown)

When the volume is a RAID array:

1. Unmount the staging path
2. Deactivate the LV: `lvchange -an <vgName>/data`
3. Deactivate the VG: `vgchange -an <vgName>`
4. Remove the VG: `vgremove -f <vgName>`
5. Remove PVs: `pvremove /dev/nvmeXn1` for each member
6. Clean up state file

### 9. Node: NodePublishVolume / NodeUnpublishVolume

No changes needed — these operate on the staging path via bind mount.

### 10. Mounter Extensions

New methods in `pkg/mounter/mount.go`:

```go
type Mounter interface {
    // Existing methods...

    // LVM operations
    CreateStripedLV(vgName string, lvName string, stripeSize string, devices []string) error
    ActivateVG(vgName string) error
    DeactivateVG(vgName string) error
    RemoveVG(vgName string) error
    RemovePVs(devices []string) error
    IsVGActive(vgName string) (bool, error)
}
```

### 11. LVM Locking

LVM commands require file locking under `/run/lock` by default. The CSI node plugin container runs with `readOnlyRootFilesystem: true` (so `/run/lock` is read-only) and without `hostPID` (blocked by Gatekeeper, so `nsenter -t 1` into host namespaces is not possible).

**Solution**: All LVM commands are run with `--nolocking`. This is safe because:
- The CSI driver is the **only LVM consumer** in the container — no other process competes for LVM locks
- Concurrent RAID operations on the same volume are serialized by the driver's `inFlight` map
- Different RAID volumes operate on disjoint sets of PVs/VGs, so no cross-volume contention

```go
func (m *NodeMounter) lvmCmd(cmd string, args ...string) ([]byte, error) {
    fullArgs := append([]string{"--nolocking"}, args...)
    return m.Exec.Command(cmd, fullArgs...).CombinedOutput()
}
```

**Alternative approaches considered**:
- **nsenter into host namespaces** (TopoLVM/OpenEBS pattern): Requires `hostPID: true`, blocked by Gatekeeper security policy
- **Create /run/lock at runtime**: `/run` is on the read-only root filesystem overlay, `mkdir` fails
- **Mount emptyDir at /run/lock**: Requires DaemonSet spec change (platform-level), not driver-controlled

### 12. Container Image Changes

The driver container image includes `lvm2`:

```dockerfile
RUN clean-apt install xfsprogs lvm2
```

## State Management

### Local State File

Each RAID volume gets a JSON state file at `/var/lib/ebs-csi-driver/raid/<composite-volume-id>.json`:

```json
{
  "compositeVolumeID": "raid0:vol-aaa+vol-bbb+vol-ccc+vol-ddd",
  "raidMode": "raid0",
  "vgName": "ebs-a1b2c3d4",
  "lvName": "data",
  "memberDevices": ["/dev/nvme1n1", "/dev/nvme2n1", "/dev/nvme3n1", "/dev/nvme4n1"],
  "stripeSize": "256K",
  "createdAt": "2026-03-06T12:00:00Z"
}
```

This enables clean teardown even if PublishContext is unavailable during unstage.

### EBS Tags

Each member volume is tagged for discoverability:

| Tag | Value |
|-----|-------|
| `raid.ebs.csi.aws.com/array-id` | PV name |
| `raid.ebs.csi.aws.com/member-index` | 0, 1, 2, ... |
| `raid.ebs.csi.aws.com/member-count` | Total member count |
| `raid.ebs.csi.aws.com/mode` | raid0 |

## Error Handling

| Scenario | Behavior |
|----------|----------|
| CreateVolume: member N fails | Roll back: delete members 0..N-1, return error |
| Attach: member N fails | Roll back: detach members 0..N-1, return error |
| DeleteVolume: member N fails (not NotFound) | Return error, retry will continue from where it left off |
| NodeStage: device not found | Return error, kubelet retries |
| NodeStage: LVM setup fails | Return error, kubelet retries |
| Node reboot mid-use | LVM metadata on PVs survives, NodeStageVolume reactivates via `vgchange -ay` |
| One member EBS fails (unlikely) | No redundancy, pod gets I/O errors |

## Capacity & Performance

For a RAID0 array with N members of type gp3:

| Metric | Single gp3 (max) | 4x RAID0 gp3 |
|--------|-------------------|---------------|
| Throughput | 1,000 MB/s | 4,000 MB/s |
| IOPS | 16,000 | 64,000 |
| Max size | 16 TiB | 64 TiB |

The driver distributes requested IOPS/throughput evenly across members. Users request the **aggregate** values in the StorageClass; the driver divides internally.

## Limitations

1. **No snapshots**: EBS snapshots are per-volume; consistent RAID0 snapshots need application-level quiesce (future work: freeze filesystem, snapshot all members, thaw)
2. **No redundancy**: Single member failure = data loss (by design — use EBS multi-AZ replication or application-level replication)
3. **Device path limits**: EC2 instances have a max number of EBS attachments (typically 28-40). A 4-member array consumes 4 attachment slots
4. **AZ-scoped**: All members must be in the same AZ (enforced by EBS)

## Volume Expansion

LVM makes online expansion feasible in a future phase:

1. Call `ModifyVolume` on each member EBS to increase size
2. Run `pvresize` on each PV to pick up new space
3. Run `lvextend -l 100%VG <vgName>/data` to grow the LV
4. Run `resize2fs` / `xfs_growfs` to expand the filesystem

This avoids the delete+recreate cycle that mdadm would require.

## Rollout

1. **Phase 1**: Core RAID0 with LVM — create, attach, stage, publish, full teardown
2. **Phase 2**: Volume expansion — online resize via LVM extend
3. **Phase 3**: Observability — metrics for per-member stats, VG health
4. **Phase 4**: Snapshot support — coordinated multi-volume snapshots

## Testing Plan

- **Unit tests**: Composite ID parsing, parameter validation, per-member capacity/IOPS splitting
- **Integration tests (mock cloud)**: Full lifecycle with mocked EC2 calls, partial failure rollback
- **E2E tests (real AWS)**: StorageClass with raidMode=raid0, verify pod sees single device with aggregate capacity, fio benchmark confirming linear throughput scaling
- **Failure tests**: Kill one member mid-I/O, verify error propagation; simulate attach failure, verify rollback

## Open Questions

1. **Host-level LVM interference between pods**: Since LVM commands run via nsenter in the host's namespaces, all VG/LV operations from different pods share the same LVM metadata and locking. Concurrent `pvcreate`/`vgcreate` calls for different RAID volumes could contend on LVM's global lock. In practice LVM handles this with its own locking (one operation at a time), but this serializes all RAID stage/unstage operations node-wide. Is this acceptable, or do we need a node-level mutex in the driver?

2. **VG name collisions**: VG names are derived from a truncated SHA256 of the composite volume ID. A hash collision would be catastrophic (two arrays sharing a VG name). The current 16-char hex prefix gives ~64 bits of collision resistance. Is this sufficient, or should we use the full hash?

3. **Orphaned host-level LVM state**: If the node plugin crashes between `pvcreate` and `vgcreate`, orphaned PVs remain on the host. LVM metadata on the PVs persists across reboots. Do we need a reconciliation loop to detect and clean up orphaned RAID state?

4. **Device-mapper visibility across namespaces**: When LVM creates a striped LV via nsenter on the host, the resulting `/dev/dm-X` device appears in the host's `/dev`. The container sees it because the node plugin mounts `/dev` from the host. But if the container's `/dev` mount is read-only or filtered, the LV device path may not be visible for `FormatAndMount`. Need to verify this works with the existing CSI node plugin's device mount configuration.

5. **lvm2 on the host**: The nsenter approach assumes `lvm2` tools are installed on the host OS. This is true for Amazon Linux 2/2023 and most Bottlerocket AMIs, but should be validated. If the host lacks `lvm2`, we fall back to container-level execution which will fail with the locking error. Should we detect this and return a clear error message?

6. **Attachment slot consumption**: A 4-member RAID0 consumes 4 of the ~28 available EBS attachment slots per instance, but Kubernetes counts it as 1 volume toward `MaxVolumesPerNode`. The CSI spec only supports a single integer limit — there's no way to report "this volume costs N slots". Three options:
   - **(a) Do nothing (current PoC)**: Let EC2 reject the attach with `AttachmentLimitExceeded`, driver returns `ResourceExhausted`, scheduler retries elsewhere. Simple, works, but the failure is late (at attach time, not scheduling time).
   - **(b) Divide limit conservatively**: Reduce `MaxVolumesPerNode` by assuming worst-case memberCount. Overly conservative, penalizes non-RAID volumes on the same node.
   - **(c) Track actual attachment count in the node plugin**: Maintain a running count of real EBS attachments (including RAID members) and reject `ControllerPublishVolume` early when slots are exhausted. This is the correct medium-term fix — it gives accurate feedback without penalizing non-RAID workloads.

7. **Leaked volumes on CreateVolume rollback failure**: If `CreateVolume` creates members 0 and 1, then member 2 fails, rollback tries to delete 0 and 1. If the rollback delete itself fails (transient EC2 error), those EBS volumes are orphaned — the composite volumeHandle was never returned to Kubernetes so nobody will ever try to clean them up. The members are tagged with `raid.ebs.csi.aws.com/array-id` so they're discoverable, but there's no reconciler to garbage-collect them. Options:
   - **(a) Accept the risk (current PoC)**: Orphans are rare (requires both a create failure AND a delete failure). Monitor via AWS Cost Explorer or tag-based alerts.
   - **(b) Retry rollback with backoff**: Before returning the error, retry the failed deletes a few times.
   - **(c) Reconciler loop**: Periodically scan for EBS volumes tagged as RAID members that don't belong to any known PV. This is the correct medium-term fix.

8. **DeleteVolume correctness**: The delete path is idempotent — it iterates all members from the composite volumeHandle, calls `DeleteDisk` on each, and tolerates `NotFound`. Partial failures are safe: on retry, already-deleted members return `NotFound` (tolerated) and the remaining members are re-attempted. No leak risk in the steady-state delete path.
