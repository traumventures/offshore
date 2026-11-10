package awsec2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"

	"github.com/traumventures/cloudsourced/internal/machine"
)

// fakeIMDS serves IMDSv2 tokens and the given metadata paths.
func fakeIMDS(t *testing.T) (*imds.Client, func(path, body string)) {
	var mu sync.Mutex
	paths := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
			w.Write([]byte("token"))
			return
		}
		if r.Header.Get("X-Aws-Ec2-Metadata-Token") != "token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		body, ok := paths[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := imds.New(imds.Options{Endpoint: srv.URL})
	return c, func(path, body string) {
		mu.Lock()
		paths["/latest/meta-data/"+path] = body
		mu.Unlock()
	}
}

func TestInterruptionFromIMDS(t *testing.T) {
	c, set := fakeIMDS(t)
	h := &host{imds: c}
	ctx := context.Background()
	n, err := h.Interruption(ctx)
	if err != nil || n != nil {
		t.Fatalf("no notice expected, got %+v %v", n, err)
	}
	set("events/recommendations/rebalance", `{"noticeTime": "2026-11-10T08:00:00Z"}`)
	n, err = h.Interruption(ctx)
	if err != nil || n == nil || n.Reason != machine.DrainSpotRebalance || n.Deadline != 0 || n.Noticed != time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("rebalance: %+v %v", n, err)
	}
	set("spot/instance-action", `{"action": "stop", "time": "2026-11-10T08:02:00Z"}`)
	n, err = h.Interruption(ctx)
	if err != nil || n == nil || n.Reason != machine.DrainSpotInterruption || n.Action != "stop" ||
		n.Deadline != time.Date(2026, 11, 10, 8, 2, 0, 0, time.UTC).Unix() {
		t.Fatalf("interruption: %+v %v", n, err)
	}
}

func TestSpotNoticeWithoutTime(t *testing.T) {
	now := time.Unix(1000, 0)
	get := func(_ context.Context, p string) ([]byte, bool, error) {
		return []byte(`{"action":"terminate"}`), p == "spot/instance-action", nil
	}
	n, err := spotNotice(context.Background(), get, now)
	if err != nil || n.Deadline != now.Add(2*time.Minute).Unix() {
		t.Fatalf("%+v %v", n, err)
	}
}
