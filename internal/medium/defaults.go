package medium

import "time"

// LocalNVMe selects the default profile of an instance-store NVMe disk
// (medium local on sizes whose instance type has one).
const LocalNVMe = "local-nvme"

const (
	mb = int64(1) << 20
	gb = int64(1) << 30
	tb = int64(1) << 40
)

// Default returns the approximate code default profile of a medium
// (Local, LocalNVMe, Volume, Store) on a provider type. All numbers are
// approximate list values (Nov 2026, us-east-1 / eu-frankfurt-1 class
// regions); measure and override them for real decisions.
func Default(providerType, med string) Profile {
	var p Profile
	switch providerType {
	case "aws-ec2":
		p = awsDefaults[med]
	case "oci":
		p = ociDefaults[med]
	case "local-docker", "static":
		p = hostDefaults[med]
		if providerType == "static" && med == Volume {
			p = Profile{}
		}
	}
	if p.Medium == "" {
		return Profile{}
	}
	p.Source = "default"
	return p
}

var awsDefaults = map[string]Profile{
	// gp3 root volume: 3000 IOPS and 125 MiB/s baseline included,
	// $0.08/GB-month; dies with the instance (DeleteOnTermination).
	// config.PoolConfig.Profile applies the provider's provisioned
	// disk_iops/disk_throughput (GP3Profile): numbers and the price of the
	// extra performance pro rata over disk_gb.
	// https://docs.aws.amazon.com/ebs/latest/userguide/general-purpose.html
	// https://aws.amazon.com/ebs/pricing/
	Local: {Medium: Local, Throughput: 125 * mb, IOPS: 3000, Latency: time.Millisecond,
		Reach: ReachMachine, Durability: ReachMachine, PricePerGBMonth: 0.08,
		MaxBytes: 16 * tb, ResizeCooldown: 6 * time.Hour, Resizable: true},
	// Instance store NVMe (m6id/c6id/i4i): GB/s, 100k+ IOPS, priced into
	// the instance, lost on stop and terminate.
	// https://docs.aws.amazon.com/ec2/latest/instancetypes/so.html#so_instance-store
	LocalNVMe: {Medium: Local, Throughput: 1500 * mb, IOPS: 100000, Latency: 100 * time.Microsecond,
		Reach: ReachMachine, Durability: ReachMachine},
	// gp3 data volume: as the root volume, plus attach (~10 s) and create
	// (~5-10 s); ModifyVolume at most once per 6 h; one AZ.
	// https://docs.aws.amazon.com/ebs/latest/userguide/modify-volume-requirements.html
	Volume: {Medium: Volume, Throughput: 125 * mb, IOPS: 3000, Latency: time.Millisecond,
		Reach: ReachZone, Durability: ReachZone, PricePerGBMonth: 0.08,
		Attach: 10 * time.Second, Provision: 10 * time.Second,
		MinBytes: gb, MaxBytes: 16 * tb, ResizeCooldown: 6 * time.Hour, Resizable: true},
	// S3 Standard: $0.023/GB-month, GET $0.0004/1000, transfer within the
	// region free, to the internet ~$0.09/GB; ~100 MB/s per connection,
	// tens of ms first byte.
	// https://aws.amazon.com/s3/pricing/
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html
	Store: {Medium: Store, Throughput: 200 * mb, Latency: 30 * time.Millisecond,
		Reach: ReachGlobal, Durability: ReachRegion, PricePerGBMonth: 0.023,
		RequestPer1k: 0.0004, EgressPerGB: 0.09},
}

var ociDefaults = map[string]Profile{
	// Boot volume, balanced (10 VPU/GB): 60 IOPS/GB, 480 KB/s per GB
	// (100 GB: 6000 IOPS, ~48 MB/s), $0.0255 + 10*$0.0017 per GB-month.
	// https://docs.oracle.com/en-us/iaas/Content/Block/Concepts/blockvolumeperformance.htm
	// https://www.oracle.com/cloud/price-list/#storage
	Local: {Medium: Local, Throughput: 48 * mb, IOPS: 6000, Latency: time.Millisecond,
		Reach: ReachMachine, Durability: ReachMachine, PricePerGBMonth: 0.0425,
		MaxBytes: 32 * tb, Resizable: true},
	// DenseIO local NVMe: GB/s, priced into the shape, lost on terminate.
	// https://docs.oracle.com/en-us/iaas/Content/Compute/References/computeshapes.htm#dense
	LocalNVMe: {Medium: Local, Throughput: 2000 * mb, IOPS: 200000, Latency: 100 * time.Microsecond,
		Reach: ReachMachine, Durability: ReachMachine},
	// Block volume, balanced: as the boot volume; paravirtualized attach
	// ~30-60 s; 50 GB..32 TB; online resize without cooldown; one AD.
	// https://docs.oracle.com/en-us/iaas/Content/Block/Tasks/resizingavolume.htm
	Volume: {Medium: Volume, Throughput: 48 * mb, IOPS: 6000, Latency: time.Millisecond,
		Reach: ReachZone, Durability: ReachZone, PricePerGBMonth: 0.0425,
		Attach: 45 * time.Second, Provision: 15 * time.Second,
		MinBytes: 50 * gb, MaxBytes: 32 * tb, Resizable: true},
	// Object Storage Standard: $0.0255/GB-month, $0.0034 per 10k requests,
	// first 10 TB/month egress free.
	// https://www.oracle.com/cloud/price-list/#storage
	Store: {Medium: Store, Throughput: 200 * mb, Latency: 30 * time.Millisecond,
		Reach: ReachGlobal, Durability: ReachRegion, PricePerGBMonth: 0.0255,
		RequestPer1k: 0.00034},
}

// local-docker and static: the host's disk; local-docker volumes are
// Docker volumes on the same disk, "attached" when a machine is created.
var hostDefaults = map[string]Profile{
	Local: {Medium: Local, Throughput: 1000 * mb, IOPS: 50000, Latency: 100 * time.Microsecond,
		Reach: ReachMachine, Durability: ReachMachine},
	LocalNVMe: {Medium: Local, Throughput: 1000 * mb, IOPS: 50000, Latency: 100 * time.Microsecond,
		Reach: ReachMachine, Durability: ReachMachine},
	Volume: {Medium: Volume, Throughput: 1000 * mb, IOPS: 50000, Latency: 100 * time.Microsecond,
		Reach: ReachZone, Durability: ReachZone, Attach: time.Second, Provision: time.Second},
	Store: {Medium: Store, Throughput: 500 * mb, Latency: time.Millisecond,
		Reach: ReachRegion, Durability: ReachRegion},
}
