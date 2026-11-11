package oci

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/traumventures/offshore/internal/provider"
)

// fakeBlock is block storage, attachments and instances in memory; state
// changes are immediate.
type fakeBlock struct {
	computeAPI
	vols   map[string]*core.Volume
	atts   map[string]*core.ParavirtualizedVolumeAttachment
	insts  []core.Instance
	n      int
	sizes  []int64
	attach []core.AttachParavirtualizedVolumeDetails
}

func newFakeBlock() *fakeBlock {
	inst := func(id, machine, ad string) core.Instance {
		return core.Instance{Id: common.String(id), AvailabilityDomain: common.String(ad), LifecycleState: core.InstanceLifecycleStateRunning,
			FreeformTags: map[string]string{tagKey(provider.LabelKind): "machine", tagKey(provider.LabelMachine): machine}}
	}
	return &fakeBlock{vols: map[string]*core.Volume{}, atts: map[string]*core.ParavirtualizedVolumeAttachment{},
		insts: []core.Instance{inst("ocid.i1", "m1", "AD-1"), inst("ocid.i2", "m2", "AD-2")}}
}

var errNotFound = svcErr{http.StatusNotFound, "NotAuthorizedOrNotFound", "not found"}

func (f *fakeBlock) ListInstances(context.Context, core.ListInstancesRequest) (core.ListInstancesResponse, error) {
	return core.ListInstancesResponse{Items: f.insts}, nil
}

func (f *fakeBlock) CreateVolume(_ context.Context, r core.CreateVolumeRequest) (core.CreateVolumeResponse, error) {
	f.n++
	d := r.CreateVolumeDetails
	v := core.Volume{Id: common.String("ocid.vol" + strconv.Itoa(f.n)), AvailabilityDomain: d.AvailabilityDomain, CompartmentId: d.CompartmentId,
		SizeInGBs: d.SizeInGBs, VpusPerGB: d.VpusPerGB, FreeformTags: d.FreeformTags, DisplayName: d.DisplayName,
		LifecycleState: core.VolumeLifecycleStateAvailable, TimeCreated: &common.SDKTime{Time: time.Now()}}
	f.vols[*v.Id] = &v
	return core.CreateVolumeResponse{Volume: v}, nil
}

func (f *fakeBlock) GetVolume(_ context.Context, r core.GetVolumeRequest) (core.GetVolumeResponse, error) {
	v, ok := f.vols[*r.VolumeId]
	if !ok {
		return core.GetVolumeResponse{}, errNotFound
	}
	return core.GetVolumeResponse{Volume: *v}, nil
}

func (f *fakeBlock) UpdateVolume(_ context.Context, r core.UpdateVolumeRequest) (core.UpdateVolumeResponse, error) {
	v := f.vols[*r.VolumeId]
	if d := r.UpdateVolumeDetails; d.SizeInGBs != nil {
		v.SizeInGBs = d.SizeInGBs
		f.sizes = append(f.sizes, *d.SizeInGBs)
	}
	if r.FreeformTags != nil {
		v.FreeformTags = r.FreeformTags
	}
	return core.UpdateVolumeResponse{Volume: *v}, nil
}

func (f *fakeBlock) DeleteVolume(_ context.Context, r core.DeleteVolumeRequest) (core.DeleteVolumeResponse, error) {
	v, ok := f.vols[*r.VolumeId]
	if !ok {
		return core.DeleteVolumeResponse{}, errNotFound
	}
	for _, a := range f.atts {
		if *a.VolumeId == *v.Id && a.LifecycleState == core.VolumeAttachmentLifecycleStateAttached {
			return core.DeleteVolumeResponse{}, svcErr{http.StatusConflict, "Conflict", "attached"}
		}
	}
	v.LifecycleState = core.VolumeLifecycleStateTerminated
	return core.DeleteVolumeResponse{}, nil
}

func (f *fakeBlock) ListVolumes(context.Context, core.ListVolumesRequest) (core.ListVolumesResponse, error) {
	var out []core.Volume
	for i := 1; i <= f.n; i++ {
		out = append(out, *f.vols["ocid.vol"+strconv.Itoa(i)])
	}
	return core.ListVolumesResponse{Items: out}, nil
}

func (f *fakeBlock) AttachVolume(_ context.Context, r core.AttachVolumeRequest) (core.AttachVolumeResponse, error) {
	d := r.AttachVolumeDetails.(core.AttachParavirtualizedVolumeDetails)
	f.attach = append(f.attach, d)
	id := "ocid.att" + strconv.Itoa(len(f.attach))
	a := &core.ParavirtualizedVolumeAttachment{Id: common.String(id), VolumeId: d.VolumeId, InstanceId: d.InstanceId, Device: d.Device,
		LifecycleState: core.VolumeAttachmentLifecycleStateAttached}
	f.atts[id] = a
	return core.AttachVolumeResponse{VolumeAttachment: *a}, nil
}

func (f *fakeBlock) DetachVolume(_ context.Context, r core.DetachVolumeRequest) (core.DetachVolumeResponse, error) {
	f.atts[*r.VolumeAttachmentId].LifecycleState = core.VolumeAttachmentLifecycleStateDetached
	return core.DetachVolumeResponse{}, nil
}

func (f *fakeBlock) GetVolumeAttachment(_ context.Context, r core.GetVolumeAttachmentRequest) (core.GetVolumeAttachmentResponse, error) {
	return core.GetVolumeAttachmentResponse{VolumeAttachment: *f.atts[*r.VolumeAttachmentId]}, nil
}

func (f *fakeBlock) ListVolumeAttachments(_ context.Context, r core.ListVolumeAttachmentsRequest) (core.ListVolumeAttachmentsResponse, error) {
	var out []core.VolumeAttachment
	for i := 1; i <= len(f.attach); i++ {
		a := f.atts["ocid.att"+strconv.Itoa(i)]
		if r.VolumeId == nil || *r.VolumeId == *a.VolumeId {
			out = append(out, *a)
		}
	}
	return core.ListVolumeAttachmentsResponse{Items: out}, nil
}

func TestVolumeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeBlock()
	o := &OCI{ready: true, infra: testInfra(), compute: f, block: f, attach: f, pollEvery: time.Millisecond}
	v, err := o.CreateVolume(ctx, provider.VolumeSpec{Owner: "pool", Name: "c1", Bytes: 20 << 30, VPUs: 20})
	if err != nil {
		t.Fatal(err)
	}
	if v.Zone != "AD-1" || v.Bytes != 50<<30 || v.State != provider.VolumeAvailable || v.Name != "c1" || v.Kind != provider.VolumeCache {
		t.Fatalf("created %+v", v)
	}
	if raw := f.vols[v.ID]; *raw.VpusPerGB != 20 || len(raw.FreeformTags) > 10 {
		t.Fatalf("volume %+v", raw)
	}
	if _, err := o.AttachVolume(ctx, v.ID, "m2"); !errors.Is(err, provider.ErrVolumeZone) {
		t.Fatalf("cross-AD attach: %v", err)
	}
	if v, err = o.AttachVolume(ctx, v.ID, "m1"); err != nil {
		t.Fatal(err)
	}
	if v.State != provider.VolumeAttached || v.Machine != "m1" || v.Device != VolumeDevice || *f.attach[0].Device != VolumeDevice {
		t.Fatalf("attached %+v", v)
	}
	if err := o.DeleteVolume(ctx, v.ID); !errors.Is(err, provider.ErrVolumeInUse) {
		t.Fatalf("delete attached: %v", err)
	}
	if err := o.ResizeVolume(ctx, v.ID, 40<<30); !errors.Is(err, provider.ErrVolumeShrink) {
		t.Fatalf("shrink: %v", err)
	}
	if err := o.ResizeVolume(ctx, v.ID, 80<<30); err != nil {
		t.Fatal(err)
	}
	if err := o.ResizeVolume(ctx, v.ID, 100<<30); err != nil || len(f.sizes) != 2 {
		t.Fatalf("oci has no resize cooldown: %v %v", err, f.sizes)
	}
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := o.DetachVolume(ctx, v.ID, exp); err != nil {
		t.Fatal(err)
	}
	vs, err := o.ListVolumes(ctx, "pool")
	if err != nil || len(vs) != 1 {
		t.Fatalf("list %v %+v", err, vs)
	}
	if g := vs[0]; g.State != provider.VolumeAvailable || g.Machine != "" || !g.ExpiresAt.Equal(exp) || g.Bytes != 100<<30 || g.ResizedAt.IsZero() {
		t.Fatalf("detached %+v", g)
	}
	if vs, _ := o.ListVolumes(ctx, "other"); len(vs) != 0 {
		t.Fatalf("other pool: %+v", vs)
	}
	if err := o.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if vs, _ := o.ListVolumes(ctx, "pool"); len(vs) != 0 {
		t.Fatalf("deleted volume listed: %+v", vs)
	}
	if _, err := o.AttachVolume(ctx, v.ID, "m1"); !errors.Is(err, provider.ErrVolumeNotFound) {
		t.Fatalf("attach deleted: %v", err)
	}
}

func TestMachineZone(t *testing.T) {
	if m := toMachine(newFakeBlock().insts[1], ""); m.Zone != "AD-2" {
		t.Fatalf("%+v", m)
	}
}
