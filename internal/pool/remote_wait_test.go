package pool

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A control plane that refuses connections (restarting) is waited for;
// the request goes through once it listens again.
func TestRemoteWaitsForControlPlane(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // refused until the server starts
	got := make(chan string, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b bytes.Buffer
			b.ReadFrom(r.Body)
			got <- b.String()
			w.Write([]byte(`{"done":true}` + "\n"))
		})}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
	}()
	r, err := NewRemote("http://"+addr, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	if err := r.callJSON(context.Background(), "/x", map[string]int{"a": 1}, &progress, nil); err != nil {
		t.Fatal(err)
	}
	if b := <-got; b != `{"a":1}` {
		t.Fatalf("body %q", b)
	}
	if !strings.Contains(progress.String(), "control plane unreachable") {
		t.Fatalf("no message: %q", progress.String())
	}

	// Without a control plane the wait is bounded.
	old := ControlPlaneWait
	ControlPlaneWait = time.Second
	defer func() { ControlPlaneWait = old }()
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln2.Addr().String()
	ln2.Close()
	r2, _ := NewRemote("http://"+dead, "", "k")
	t0 := time.Now()
	if err := r2.callJSON(context.Background(), "/x", nil, &progress, nil); err == nil || time.Since(t0) > 5*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(t0))
	}
}
