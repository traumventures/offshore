package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Call sends one request to the daemon, forwarding progress lines.
func Call(ctx context.Context, req Request, progress io.Writer) (json.RawMessage, error) {
	var dialer net.Dialer
	c, err := dialer.DialContext(ctx, "unix", SocketPath())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var resp Response
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			return nil, err
		}
		if resp.Progress != "" && progress != nil {
			io.WriteString(progress, resp.Progress)
		}
		if resp.Done {
			if resp.Error != "" {
				return nil, errors.New(resp.Error)
			}
			return resp.Result, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("daemon closed the connection")
}

// EnsureDaemon starts the daemon in the background if it is not running.
func EnsureDaemon(ctx context.Context) error {
	if _, err := Call(ctx, Request{Op: "ping"}, nil); err == nil {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "daemon", "run")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Call(ctx, Request{Op: "ping"}, nil); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start (see %s)", LogPath())
}
