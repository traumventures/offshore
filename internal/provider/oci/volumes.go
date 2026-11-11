package oci

import (
	"context"
	"fmt"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/traumventures/offshore/internal/provider"
)

// blockAPI is the part of the block storage client the provider uses.
type blockAPI interface {
	CreateVolume(ctx context.Context, req core.CreateVolumeRequest) (core.CreateVolumeResponse, error)
	GetVolume(ctx context.Context, req core.GetVolumeRequest) (core.GetVolumeResponse, error)
	UpdateVolume(ctx context.Context, req core.UpdateVolumeRequest) (core.UpdateVolumeResponse, error)
	DeleteVolume(ctx context.Context, req core.DeleteVolumeRequest) (core.DeleteVolumeResponse, error)
	ListVolumes(ctx context.Context, req core.ListVolumesRequest) (core.ListVolumesResponse, error)
}

// attachAPI is the part of the compute client that attaches volumes.
type attachAPI interface {
	AttachVolume(ctx context.Context, req core.AttachVolumeRequest) (core.AttachVolumeResponse, error)
	DetachVolume(ctx context.Context, req core.DetachVolumeRequest) (core.DetachVolumeResponse, error)
	GetVolumeAttachment(ctx context.Context, req core.GetVolumeAttachmentRequest) (core.GetVolumeAttachmentResponse, error)
	ListVolumeAttachments(ctx context.Context, req core.ListVolumeAttachmentsRequest) (core.ListVolumeAttachmentsResponse, error)
}

var _ provider.Volumes = (*OCI)(nil)

// VolumeDevice is the consistent device path of the cache volume on an
// instance (paravirtualized attachment; Oracle platform images create
// the /dev/oracleoci links).
const VolumeDevice = "/dev/oracleoci/oraclevdb"

// volumeWait bounds the waits for volume and attachment state changes.
var volumeWait = 10 * time.Minute

// VolumeCaps implements provider.Volumes: block volumes of 50 GB..32 TB
// in one availability domain, attached paravirtualized to running or
// stopped instances, resizable online without a cooldown.
func (o *OCI) VolumeCaps() provider.VolumeCaps {
	return provider.VolumeCaps{MinBytes: 50 << 30, MaxBytes: 32 << 40, HotAttach: true, Zonal: true, Bounded: true}
}

// volumeTags maps volume labels to freeform tags (see setLabel).
func volumeTags(l map[string]string) map[string]string {
	t := map[string]string{tagProject: project}
	for _, k := range provider.SortedKeys(l) {
		setLabel(t, k, l[k])
	}
	return t
}

// CreateVolume implements provider.Volumes.
func (o *OCI) CreateVolume(ctx context.Context, spec provider.VolumeSpec) (provider.Volume, error) {
	if err := o.init(); err != nil {
		return provider.Volume{}, err
	}
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return provider.Volume{}, err
	}
	ad := spec.Zone
	if ad == "" {
		ad = in.ad
	}
	gb := max(provider.GiB(spec.Bytes), 50)
	d := core.CreateVolumeDetails{
		CompartmentId: common.String(in.compartment), AvailabilityDomain: common.String(ad),
		DisplayName: common.String("offshore-" + spec.Name), SizeInGBs: common.Int64(gb),
		FreeformTags: volumeTags(provider.VolumeLabels(spec, time.Now())),
	}
	if spec.VPUs > 0 {
		d.VpusPerGB = common.Int64(int64(spec.VPUs))
	}
	resp, err := o.block.CreateVolume(ctx, core.CreateVolumeRequest{CreateVolumeDetails: d})
	if err != nil {
		return provider.Volume{}, fmt.Errorf("oci: create volume %s: %w", spec.Name, err)
	}
	id := deref(resp.Id)
	var v core.Volume
	err = o.poll(ctx, volumeWait, func() (bool, error) {
		r, err := o.block.GetVolume(ctx, core.GetVolumeRequest{VolumeId: common.String(id)})
		if err != nil {
			return false, err
		}
		v = r.Volume
		switch v.LifecycleState {
		case core.VolumeLifecycleStateAvailable:
			return true, nil
		case core.VolumeLifecycleStateFaulty, core.VolumeLifecycleStateTerminated:
			return false, fmt.Errorf("volume %s", v.LifecycleState)
		}
		return false, nil
	})
	if err != nil {
		return provider.Volume{}, fmt.Errorf("oci: volume %s not available: %w", id, err)
	}
	return toVolume(v, nil), nil
}

// AttachVolume implements provider.Volumes (paravirtualized, at
// VolumeDevice).
func (o *OCI) AttachVolume(ctx context.Context, id, machineID string) (provider.Volume, error) {
	if err := o.init(); err != nil {
		return provider.Volume{}, err
	}
	v, err := o.volume(ctx, id)
	if err != nil {
		return provider.Volume{}, err
	}
	inst, err := o.byInstance(ctx, machineID)
	if err != nil {
		return provider.Volume{}, err
	}
	if ad := deref(inst.AvailabilityDomain); ad != v.Zone {
		return provider.Volume{}, fmt.Errorf("oci: attach %s (%s) to %s (%s): %w", id, v.Zone, machineID, ad, provider.ErrVolumeZone)
	}
	if v.State == provider.VolumeAttached && v.Machine == machineID {
		return v, nil
	}
	if v.State != provider.VolumeAvailable {
		return provider.Volume{}, fmt.Errorf("oci: attach %s: %w (%s)", id, provider.ErrVolumeInUse, v.State)
	}
	resp, err := o.attach.AttachVolume(ctx, core.AttachVolumeRequest{AttachVolumeDetails: core.AttachParavirtualizedVolumeDetails{
		InstanceId: inst.Id, VolumeId: common.String(id), Device: common.String(VolumeDevice),
		DisplayName: common.String("offshore-cache"), IsReadOnly: common.Bool(false),
	}})
	if err != nil {
		if conflict(err) {
			err = fmt.Errorf("%w: %v", provider.ErrVolumeInUse, err)
		}
		return provider.Volume{}, fmt.Errorf("oci: attach %s to %s: %w", id, machineID, err)
	}
	if err := o.waitAttachment(ctx, deref(resp.VolumeAttachment.GetId()), core.VolumeAttachmentLifecycleStateAttached); err != nil {
		return provider.Volume{}, err
	}
	l := v.Labels
	l[provider.LabelMachine] = machineID
	delete(l, provider.LabelExpiresAt)
	if err := o.setVolumeLabels(ctx, id, l); err != nil {
		return provider.Volume{}, err
	}
	return o.volume(ctx, id)
}

func (o *OCI) waitAttachment(ctx context.Context, id string, want core.VolumeAttachmentLifecycleStateEnum) error {
	err := o.poll(ctx, volumeWait, func() (bool, error) {
		r, err := o.attach.GetVolumeAttachment(ctx, core.GetVolumeAttachmentRequest{VolumeAttachmentId: common.String(id)})
		if err != nil {
			return false, err
		}
		return r.VolumeAttachment.GetLifecycleState() == want, nil
	})
	if err != nil {
		return fmt.Errorf("oci: volume attachment %s not %s: %w", id, want, err)
	}
	return nil
}

// DetachVolume implements provider.Volumes.
func (o *OCI) DetachVolume(ctx context.Context, id string, expiresAt time.Time) error {
	if err := o.init(); err != nil {
		return err
	}
	v, err := o.volume(ctx, id)
	if err != nil {
		return err
	}
	atts, err := o.attachments(ctx, id)
	if err != nil {
		return err
	}
	for _, a := range atts {
		switch a.GetLifecycleState() {
		case core.VolumeAttachmentLifecycleStateAttaching, core.VolumeAttachmentLifecycleStateAttached:
			if _, err := o.attach.DetachVolume(ctx, core.DetachVolumeRequest{VolumeAttachmentId: a.GetId()}); err != nil && !notFound(err) {
				return fmt.Errorf("oci: detach %s: %w", id, err)
			}
			fallthrough
		case core.VolumeAttachmentLifecycleStateDetaching:
			if err := o.waitAttachment(ctx, deref(a.GetId()), core.VolumeAttachmentLifecycleStateDetached); err != nil {
				return err
			}
		}
	}
	l := v.Labels
	delete(l, provider.LabelMachine)
	delete(l, provider.LabelExpiresAt)
	if !expiresAt.IsZero() {
		l[provider.LabelExpiresAt] = provider.UnixLabel(expiresAt)
	}
	return o.setVolumeLabels(ctx, id, l)
}

// ResizeVolume implements provider.Volumes (online; the supervisor
// rescans the device and grows the file system).
func (o *OCI) ResizeVolume(ctx context.Context, id string, bytes int64) error {
	if err := o.init(); err != nil {
		return err
	}
	v, err := o.volume(ctx, id)
	if err != nil {
		return err
	}
	gb := provider.GiB(bytes)
	if err := provider.CheckResize(o.VolumeCaps(), v, gb<<30, time.Now()); err != nil {
		return fmt.Errorf("oci: resize %s to %d GB: %w", id, gb, err)
	}
	l := v.Labels
	l[provider.LabelResizedAt] = provider.UnixLabel(time.Now())
	_, err = o.block.UpdateVolume(ctx, core.UpdateVolumeRequest{VolumeId: common.String(id),
		UpdateVolumeDetails: core.UpdateVolumeDetails{SizeInGBs: common.Int64(gb), FreeformTags: volumeTags(l)}})
	if err != nil {
		return fmt.Errorf("oci: resize %s to %d GB: %w", id, gb, err)
	}
	return nil
}

// DeleteVolume implements provider.Volumes.
func (o *OCI) DeleteVolume(ctx context.Context, id string) error {
	if err := o.init(); err != nil {
		return err
	}
	_, err := o.block.DeleteVolume(ctx, core.DeleteVolumeRequest{VolumeId: common.String(id)})
	switch {
	case err == nil, notFound(err):
		return nil
	case conflict(err):
		return fmt.Errorf("oci: delete %s: %w", id, provider.ErrVolumeInUse)
	}
	return fmt.Errorf("oci: delete %s: %w", id, err)
}

// ListVolumes implements provider.Volumes.
func (o *OCI) ListVolumes(ctx context.Context, owner string) ([]provider.Volume, error) {
	if err := o.init(); err != nil {
		return nil, err
	}
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return nil, err
	}
	attached := map[string]core.VolumeAttachment{}
	areq := core.ListVolumeAttachmentsRequest{CompartmentId: common.String(in.compartment)}
	for {
		r, err := o.attach.ListVolumeAttachments(ctx, areq)
		if err != nil {
			return nil, fmt.Errorf("oci: list volume attachments: %w", err)
		}
		for _, a := range r.Items {
			if s := a.GetLifecycleState(); s != core.VolumeAttachmentLifecycleStateDetached {
				attached[deref(a.GetVolumeId())] = a
			}
		}
		if r.OpcNextPage == nil {
			break
		}
		areq.Page = r.OpcNextPage
	}
	var out []provider.Volume
	req := core.ListVolumesRequest{CompartmentId: common.String(in.compartment)}
	for {
		r, err := o.block.ListVolumes(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("oci: list volumes: %w", err)
		}
		for _, v := range r.Items {
			t := v.FreeformTags
			if v.LifecycleState == core.VolumeLifecycleStateTerminated || t[tagProject] != project ||
				(owner != "" && t[tagKey(provider.LabelPool)] != owner) || labels(t)[provider.LabelVolumeName] == "" {
				continue
			}
			out = append(out, toVolume(v, attached[deref(v.Id)]))
		}
		if r.OpcNextPage == nil {
			return out, nil
		}
		req.Page = r.OpcNextPage
	}
}

func (o *OCI) volume(ctx context.Context, id string) (provider.Volume, error) {
	r, err := o.block.GetVolume(ctx, core.GetVolumeRequest{VolumeId: common.String(id)})
	if err != nil {
		if notFound(err) {
			return provider.Volume{}, fmt.Errorf("oci: %s: %w", id, provider.ErrVolumeNotFound)
		}
		return provider.Volume{}, fmt.Errorf("oci: volume %s: %w", id, err)
	}
	if r.LifecycleState == core.VolumeLifecycleStateTerminated {
		return provider.Volume{}, fmt.Errorf("oci: %s: %w", id, provider.ErrVolumeNotFound)
	}
	atts, err := o.attachments(ctx, id)
	if err != nil {
		return provider.Volume{}, err
	}
	var att core.VolumeAttachment
	for _, a := range atts {
		if a.GetLifecycleState() != core.VolumeAttachmentLifecycleStateDetached {
			att = a
		}
	}
	return toVolume(r.Volume, att), nil
}

func (o *OCI) attachments(ctx context.Context, volumeID string) ([]core.VolumeAttachment, error) {
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return nil, err
	}
	r, err := o.attach.ListVolumeAttachments(ctx, core.ListVolumeAttachmentsRequest{CompartmentId: common.String(in.compartment), VolumeId: common.String(volumeID)})
	if err != nil {
		return nil, fmt.Errorf("oci: volume attachments of %s: %w", volumeID, err)
	}
	return r.Items, nil
}

func (o *OCI) setVolumeLabels(ctx context.Context, id string, l map[string]string) error {
	_, err := o.block.UpdateVolume(ctx, core.UpdateVolumeRequest{VolumeId: common.String(id),
		UpdateVolumeDetails: core.UpdateVolumeDetails{FreeformTags: volumeTags(l)}})
	if err != nil {
		return fmt.Errorf("oci: tag volume %s: %w", id, err)
	}
	return nil
}

// toVolume maps a block volume and its live attachment (nil: none).
func toVolume(v core.Volume, att core.VolumeAttachment) provider.Volume {
	l := labels(v.FreeformTags)
	delete(l, tagProject)
	pv := provider.Volume{ID: deref(v.Id), Zone: deref(v.AvailabilityDomain), Labels: l}
	if v.SizeInGBs != nil {
		pv.Bytes = *v.SizeInGBs << 30
	}
	if v.TimeCreated != nil {
		pv.CreatedAt = v.TimeCreated.Time
	}
	provider.VolumeFromLabels(&pv)
	switch v.LifecycleState {
	case core.VolumeLifecycleStateProvisioning, core.VolumeLifecycleStateRestoring:
		pv.State = provider.VolumeCreating
	case core.VolumeLifecycleStateAvailable:
		pv.State = provider.VolumeAvailable
		if att != nil {
			pv.State = provider.VolumeBusy
			if att.GetLifecycleState() == core.VolumeAttachmentLifecycleStateAttached {
				pv.State = provider.VolumeAttached
				pv.Machine = l[provider.LabelMachine]
				pv.Device = deref(att.GetDevice())
			}
		}
	case core.VolumeLifecycleStateFaulty:
		pv.State = provider.VolumeFailed
	default:
		pv.State = provider.VolumeBusy
	}
	return pv
}
