//go:build ocismoke

// Live test of the native store (pre-authenticated requests) against a real
// tenancy (~/.oci/config profile DEFAULT, infrastructure of the oci
// provider); never part of the regular test run:
//
//	go test -tags ocismoke -run TestOCIStoreSmoke -v ./internal/provider/oci

package oci

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/offshore/internal/store"
)

func TestOCIStoreSmoke(t *testing.T) {
	p, err := open(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	o := p.(*OCI)
	ctx := context.Background()
	g, ok, err := o.NativeGranter(ctx, "oci", "smoke")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	pg := g.(*parGranter)
	gr, err := g.Grant(ctx, []string{store.RepoScope("r1")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("grant %s expires %s", gr.URLs["repos/r1"][:60]+"...", time.Unix(gr.Expires, 0))
	defer func() {
		for _, name := range []string{"cache/smoke/repos/r1/chunks/ab/cd"} {
			o.objects.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: &pg.namespace, BucketName: &pg.bucket, ObjectName: common.String(name)})
		}
		pg.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
		pg.sweep(ctx, time.Now().Add(48*time.Hour))
	}()
	st, err := store.Open(gr, "repos/r1/chunks", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Stat(ctx, "ab/cd"); err == nil {
		t.Fatal("object exists before put")
	}
	if err := st.Put(ctx, "ab/cd", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "ab/cd", strings.NewReader("hello"), -1); err != nil {
		t.Fatal("overwrite:", err)
	}
	rc, info, err := st.Get(ctx, "ab/cd")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" || info.Size != 5 {
		t.Fatalf("get %q %+v", b, info)
	}
	if _, err := st.Stat(ctx, "ab/cd"); err != nil {
		t.Fatal("stat:", err)
	}
	// Outside the prefix: refused.
	other := strings.Replace(gr.URLs["repos/r1"], "/repos/r1/", "/repos/r2/", 1) + "x"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, other, strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	t.Logf("PUT outside the prefix: %s", resp.Status)
	if resp.StatusCode/100 == 2 {
		t.Fatal("write outside the granted prefix succeeded")
	}
	// Listing denied.
	base := gr.URLs["repos/r1"][:strings.Index(gr.URLs["repos/r1"], "/o/")+3]
	resp, err = http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	t.Logf("GET listing: %s", resp.Status)
	if resp.StatusCode/100 == 2 {
		t.Fatal("listing allowed")
	}
}
