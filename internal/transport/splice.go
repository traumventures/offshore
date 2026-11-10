package transport

import (
	"io"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

// Splice copies between a TCP connection and a stream in both directions,
// propagating half closes, until both directions are done.
func Splice(c net.Conn, st *quic.Stream) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(st, c)
		st.Close() // our send side; the peer sees EOF
	}()
	go func() {
		defer wg.Done()
		io.Copy(c, st)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			c.Close()
		}
	}()
	wg.Wait()
}
