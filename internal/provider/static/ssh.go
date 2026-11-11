package static

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/traumventures/offshore/internal/config"
)

// sshTransport runs commands over SSH with a key and verified host keys.
type sshTransport struct {
	signer ssh.Signer
	hostCB ssh.HostKeyCallback
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func newSSH(keyRef, knownHosts string) (*sshTransport, error) {
	if kind, path, ok := strings.Cut(keyRef, ":"); ok && kind == "file" {
		keyRef = "file:" + expandHome(path)
	}
	key, err := config.ResolveRef(keyRef, "")
	if err != nil {
		return nil, fmt.Errorf("static: ssh_key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("static: ssh_key: %w", err)
	}
	cb, err := knownhosts.New(expandHome(knownHosts))
	if err != nil {
		return nil, fmt.Errorf("static: known_hosts: %w", err)
	}
	return &sshTransport{signer: signer, hostCB: cb}, nil
}

func (t *sshTransport) Run(ctx context.Context, h Host, cmd string, stdin []byte) ([]byte, error) {
	user, addr, ok := strings.Cut(h.SSH, "@")
	if !ok {
		user, addr = "root", h.SSH
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(t.signer)}, HostKeyCallback: t.hostCB, Timeout: 15 * time.Second}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &out
	err = sess.Run(cmd)
	return out.Bytes(), err
}
