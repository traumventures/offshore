#!/bin/sh
# Fresh-slot commit benchmark (anyfsbench slotcommit) on a gp3-like disk:
# an ext4 loop device throttled by cgroup io.max (rbps/wbps), the store a
# directory on the host disk behind a simulated upload bandwidth.
#
#   sudo sh slotcommit.sh MNT LOOPDEV STORE BYTES [DISK_BPS] [STORE_BPS] [RATE] > results.jsonl
#
# MNT: mount point of LOOPDEV (created with losetup --direct-io=on).
set -eu
mnt=$1 dev=$2 store=$3 bytes=$4 disk=${5:-125M} up=${6:-500e6} rate=${7:-40e6}
bin=${BIN:-/tmp/anyfsbench}
for mode in ${MODES:-before after}; do
	rm -rf "$mnt/run" "$store"
	mkdir -p "$mnt/run" "$store"
	sync
	systemd-run --quiet --scope -p "IOReadBandwidthMax=$dev $disk" -p "IOWriteBandwidthMax=$dev $disk" \
		"$bin" slotcommit -mode "$mode" -upper "$mnt/run/upper" -cache "$mnt/run/cache" -store "$store" \
		-bytes "$bytes" -bw "$up" -rate "$rate"
done
rm -rf "$mnt/run" "$store"
