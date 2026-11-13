package medium

// gp3 pricing (us-east-1 list prices, Nov 2026): storage per GB-month;
// 3000 IOPS and 125 MiB/s are included, provisioned performance above
// that is billed per IOPS-month and per MiB/s-month.
// https://aws.amazon.com/ebs/pricing/
const (
	GP3PricePerGBMonth         = 0.08
	GP3BaselineIOPS            = 3000
	GP3BaselineThroughput      = 125 // MiB/s
	GP3PricePerIOPSMonth       = 0.005
	GP3PricePerThroughputMonth = 0.04 // per MiB/s-month
)

// Default root volume of the aws-ec2 provider (options disk_gb,
// disk_iops, disk_throughput).
const (
	AWSRootDiskGB         = 100
	AWSRootDiskIOPS       = 6000
	AWSRootDiskThroughput = 500 // MiB/s
)

// GP3PricePerGBMonthOf is the full price per GB-month of a gp3 volume of
// gb GiB with the given provisioned IOPS and throughput (MiB/s): the
// storage price plus the provisioned performance above the baseline,
// spread pro rata over the volume's size. A stopped machine keeps paying
// all of it.
func GP3PricePerGBMonthOf(gb, iops, throughput int64) float64 {
	p := GP3PricePerGBMonth
	if gb <= 0 {
		return p
	}
	extra := float64(max(iops-GP3BaselineIOPS, 0))*GP3PricePerIOPSMonth +
		float64(max(throughput-GP3BaselineThroughput, 0))*GP3PricePerThroughputMonth
	return p + extra/float64(gb)
}

// GP3Profile returns the profile p (a gp3 root volume's) with the
// performance and price of a volume of gb GiB, iops and throughput
// (MiB/s); non-positive values keep p's numbers.
func GP3Profile(p Profile, gb, iops, throughput int64) Profile {
	if iops > 0 {
		p.IOPS = int(iops)
	} else {
		iops = int64(p.IOPS)
	}
	if throughput > 0 {
		p.Throughput = throughput * mb
	} else {
		throughput = p.Throughput / mb
	}
	p.PricePerGBMonth = GP3PricePerGBMonthOf(gb, iops, throughput)
	return p
}
