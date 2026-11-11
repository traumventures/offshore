package localdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/provider"
)

// fakeCLI answers the Docker CLI calls of the volume operations.
type fakeCLI struct {
	vols   map[string]map[string]string // name -> labels
	mounts map[string][]string          // machine id -> mounted volumes
}

func (f *fakeCLI) exec(_ context.Context, out io.Writer, args []string) error {
	switch strings.Join(args[:2], " ") {
	case "volume create":
		name := args[len(args)-1]
		l := map[string]string{}
		for i := 2; i < len(args)-1; i += 2 {
			k, v, _ := strings.Cut(args[i+1], "=")
			l[k] = v
		}
		f.vols[name] = l
	case "volume inspect":
		l, ok := f.vols[args[len(args)-1]]
		if !ok {
			return errors.New("docker volume: Error: No such volume")
		}
		return json.NewEncoder(out).Encode(l)
	case "volume ls":
		var names []string
		for n := range f.vols {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			var kv []string
			for _, k := range provider.SortedKeys(f.vols[n]) {
				kv = append(kv, k+"="+f.vols[n][k])
			}
			fmt.Fprintf(out, "%s\t%s\n", n, strings.Join(kv, ","))
		}
	case "volume rm":
		name := args[len(args)-1]
		for _, ms := range f.mounts {
			for _, m := range ms {
				if m == name {
					return errors.New("docker volume: Error: volume is in use")
				}
			}
		}
		if _, ok := f.vols[name]; !ok {
			return errors.New("docker volume: Error: No such volume")
		}
		delete(f.vols, name)
	case "ps -a":
		for id, ms := range f.mounts {
			fmt.Fprintf(out, "%s\t%s\n", id, strings.Join(ms, ","))
		}
	default:
		return fmt.Errorf("unexpected docker %v", args)
	}
	return nil
}

func TestVolumes(t *testing.T) {
	ctx := context.Background()
	f := &fakeCLI{vols: map[string]map[string]string{}, mounts: map[string][]string{}}
	d := &Docker{Exec: f.exec, StateDir: t.TempDir()}
	if _, err := d.CreateVolume(ctx, provider.VolumeSpec{Owner: "p", Name: "c1", Zone: "eu-1a"}); !errors.Is(err, provider.ErrVolumeZone) {
		t.Fatalf("zone: %v", err)
	}
	v, err := d.CreateVolume(ctx, provider.VolumeSpec{Owner: "p", Name: "c1", Bytes: 10 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "cs-vol-p-c1" || v.Zone != Zone || v.State != provider.VolumeAvailable || v.Name != "c1" || v.Kind != provider.VolumeCache {
		t.Fatalf("created %+v", v)
	}
	if err := d.checkAttachable(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	f.mounts["m1"] = []string{v.ID, "cs-deps-p-1"}
	if _, err := d.AttachVolume(ctx, v.ID, "m2"); !errors.Is(err, provider.ErrNoHotAttach) {
		t.Fatalf("hot attach: %v", err)
	}
	if got, err := d.AttachVolume(ctx, v.ID, "m1"); err != nil || got.Machine != "m1" || got.State != provider.VolumeAttached {
		t.Fatalf("attach at creation: %v %+v", err, got)
	}
	if err := d.checkAttachable(ctx, v.ID); !errors.Is(err, provider.ErrVolumeInUse) {
		t.Fatalf("second machine: %v", err)
	}
	if err := d.DetachVolume(ctx, v.ID, time.Now()); !errors.Is(err, provider.ErrNoHotAttach) {
		t.Fatalf("detach from live machine: %v", err)
	}
	if err := d.DeleteVolume(ctx, v.ID); !errors.Is(err, provider.ErrVolumeInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if err := d.ResizeVolume(ctx, v.ID, 1<<40); err != nil {
		t.Fatal(err)
	}
	delete(f.mounts, "m1") // machine destroyed
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := d.DetachVolume(ctx, v.ID, exp); err != nil {
		t.Fatal(err)
	}
	vs, err := d.ListVolumes(ctx, "p")
	if err != nil || len(vs) != 1 || !vs[0].ExpiresAt.Equal(exp) || vs[0].Machine != "" {
		t.Fatalf("list %v %+v", err, vs)
	}
	// Using it again clears the expiry.
	if err := d.checkAttachable(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if vs, _ := d.ListVolumes(ctx, "p"); !vs[0].ExpiresAt.IsZero() {
		t.Fatalf("expiry kept: %+v", vs[0])
	}
	if err := d.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := d.AttachVolume(ctx, v.ID, "m1"); !errors.Is(err, provider.ErrVolumeNotFound) {
		t.Fatalf("attach deleted: %v", err)
	}
}
