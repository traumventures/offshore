package anyfs

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// AccessWatch reports the files closed on a file system (fanotify,
// CAP_SYS_ADMIN): every file a process other than this one closed after
// reading (write false) or writing it. It marks the whole file system
// (super block), so accesses through bind mounts in other mount
// namespaces (sessions) are seen too; their paths are those of the
// accessing mount namespace.
type AccessWatch struct {
	fd   int
	stop chan struct{}
	done chan struct{}
	once sync.Once
	// Lost counts queue overflows (accesses missed).
	lost atomic.Int64
}

// WatchAccess starts watching the file system holding dir; on runs on a
// single goroutine with each closed file's path.
func WatchAccess(dir string, on func(path string, write bool)) (*AccessWatch, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK, unix.O_RDONLY|unix.O_LARGEFILE|unix.O_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("fanotify: %w", err)
	}
	if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, unix.FAN_CLOSE_NOWRITE|unix.FAN_CLOSE_WRITE, unix.AT_FDCWD, dir); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fanotify mark %s: %w", dir, err)
	}
	w := &AccessWatch{fd: fd, stop: make(chan struct{}), done: make(chan struct{})}
	go w.loop(on)
	return w, nil
}

// Lost reports how many times the event queue overflowed.
func (w *AccessWatch) Lost() int64 { return w.lost.Load() }

// Close stops the watch.
func (w *AccessWatch) Close() {
	w.once.Do(func() {
		close(w.stop)
		<-w.done
		unix.Close(w.fd)
	})
}

func (w *AccessWatch) loop(on func(string, bool)) {
	defer close(w.done)
	self := int32(os.Getpid())
	buf := make([]byte, 64<<10)
	const metaLen = int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))
	pfd := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
	for {
		select {
		case <-w.stop:
			return
		default:
		}
		if _, err := unix.Poll(pfd, 200); err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		n, err := unix.Read(w.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		for off := 0; off+metaLen <= n; {
			m := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			if m.Event_len < uint32(metaLen) {
				break
			}
			off += int(m.Event_len)
			if m.Mask&unix.FAN_Q_OVERFLOW != 0 {
				w.lost.Add(1)
			}
			if m.Fd < 0 {
				continue
			}
			if m.Pid != self {
				if p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(m.Fd))); err == nil {
					on(p, m.Mask&unix.FAN_CLOSE_WRITE != 0)
				}
			}
			unix.Close(int(m.Fd))
		}
	}
}
