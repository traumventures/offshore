package snapshot

import "golang.org/x/sys/unix"

// lutimes sets the times of a path without following a symlink.
func lutimesNano(p string, nsec int64) error {
	ts := []unix.Timespec{unix.NsecToTimespec(nsec), unix.NsecToTimespec(nsec)}
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
}
