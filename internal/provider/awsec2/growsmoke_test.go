//go:build storagesmoke

// Live check of the background grow on a disposable EC2 machine: after
// the root volume was enlarged (ResizeDisk, ModifyVolume), GrowDisk grows
// the partition and file system online. Run as root:
//
//	SMOKE_GROW_GB=<new size> ./awsec2-smoke.test -test.run TestLiveGrowDisk -test.v

package awsec2

import (
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestLiveGrowDisk(t *testing.T) {
	want, _ := strconv.ParseInt(os.Getenv("SMOKE_GROW_GB"), 10, 64)
	if want == 0 || os.Geteuid() != 0 {
		t.Skip("SMOKE_GROW_GB and root required")
	}
	size := func() int64 {
		var st syscall.Statfs_t
		if err := syscall.Statfs("/", &st); err != nil {
			t.Fatal(err)
		}
		return int64(st.Blocks) * st.Bsize
	}
	before := size()
	h := &host{}
	// The volume grows within seconds of ModifyVolume, the block device
	// may lag: GrowDisk is retried like the supervisor's grow loop.
	deadline := time.Now().Add(5 * time.Minute)
	for size() < (want-2)<<30 {
		if err := h.GrowDisk(t.Context()); err != nil {
			t.Logf("grow: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("file system %d GiB, want ~%d GiB", size()>>30, want)
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("root file system grew from %d GiB to %d GiB", before>>30, size()>>30)
}
