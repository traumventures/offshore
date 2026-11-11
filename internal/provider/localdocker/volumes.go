package localdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/traumventures/offshore/internal/provider"
)

// Local volumes are Docker volumes: unbounded directories on the host,
// all in one zone ("local"). A container gets its volumes when it is
// created, so a volume attaches only at machine creation
// (MachineSpec.Volume) and detaches when the machine is destroyed.
// Docker volume labels are immutable: the expiry set at detach lives in
// a state file.

// Zone is the only zone of local-docker.
const Zone = "local"

var _ provider.Volumes = (*Docker)(nil)

// VolumeCaps implements provider.Volumes.
func (d *Docker) VolumeCaps() provider.VolumeCaps { return provider.VolumeCaps{} }

// volumeName is the Docker volume of an offshore volume name.
func volumeName(owner, name string) string { return "cs-vol-" + sanitize(owner) + "-" + sanitize(name) }

// CreateVolume implements provider.Volumes.
func (d *Docker) CreateVolume(ctx context.Context, spec provider.VolumeSpec) (provider.Volume, error) {
	if spec.Zone != "" && spec.Zone != Zone {
		return provider.Volume{}, fmt.Errorf("local-docker: zone %q: %w", spec.Zone, provider.ErrVolumeZone)
	}
	name := volumeName(spec.Owner, spec.Name)
	l := provider.VolumeLabels(spec, time.Now())
	if err := d.run(ctx, nil, append(append([]string{"volume", "create"}, labels(l)...), name)...); err != nil {
		return provider.Volume{}, err
	}
	return d.volume(ctx, name)
}

// AttachVolume implements provider.Volumes: only the machine created
// with the volume has it.
func (d *Docker) AttachVolume(ctx context.Context, id, machineID string) (provider.Volume, error) {
	v, err := d.volume(ctx, id)
	if err != nil {
		return provider.Volume{}, err
	}
	if v.Machine == machineID {
		return v, nil
	}
	return provider.Volume{}, fmt.Errorf("local-docker: attach %s to %s: %w", id, machineID, provider.ErrNoHotAttach)
}

// DetachVolume implements provider.Volumes: the volume must be unused
// (its machine destroyed); the expiry goes to the state file.
func (d *Docker) DetachVolume(ctx context.Context, id string, expiresAt time.Time) error {
	v, err := d.volume(ctx, id)
	if err != nil {
		return err
	}
	if v.Machine != "" {
		return fmt.Errorf("local-docker: detach %s from %s: %w", id, v.Machine, provider.ErrNoHotAttach)
	}
	return d.updateState(func(st map[string]volState) {
		if expiresAt.IsZero() {
			delete(st, id)
		} else {
			st[id] = volState{ExpiresAt: expiresAt.Unix()}
		}
	})
}

// ResizeVolume implements provider.Volumes: local volumes have no fixed
// size.
func (d *Docker) ResizeVolume(ctx context.Context, id string, bytes int64) error {
	_, err := d.volume(ctx, id)
	return err
}

// DeleteVolume implements provider.Volumes. Volumes in use are left alone.
func (d *Docker) DeleteVolume(ctx context.Context, id string) error {
	err := d.run(ctx, nil, "volume", "rm", id)
	switch {
	case err == nil:
	case strings.Contains(err.Error(), "in use"):
		return fmt.Errorf("local-docker: delete %s: %w", id, provider.ErrVolumeInUse)
	case strings.Contains(strings.ToLower(err.Error()), "no such volume"):
	default:
		return err
	}
	return d.updateState(func(st map[string]volState) { delete(st, id) })
}

// ListVolumes implements provider.Volumes: all volumes of owner (cache
// volumes, per-machine cache volumes, shared dependency caches).
func (d *Docker) ListVolumes(ctx context.Context, owner string) ([]provider.Volume, error) {
	// One listing with the labels: inspecting the listed names separately
	// fails as a whole when a volume disappears in between (a concurrent
	// destroy), which made reconciles fail.
	out, err := d.output(ctx, "volume", "ls", "--filter", "label="+provider.LabelPool+"="+owner, "--format", "{{.Name}}\t{{.Labels}}")
	if err != nil {
		return nil, err
	}
	users, err := d.volumeUsers(ctx, owner)
	if err != nil {
		return nil, err
	}
	st := d.readState()
	var vs []provider.Volume
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, lbls, _ := strings.Cut(line, "\t")
		if name == "" {
			continue
		}
		vs = append(vs, toVolume(name, parseLabels(lbls), users[name], st[name]))
	}
	return vs, nil
}

func (d *Docker) volume(ctx context.Context, id string) (provider.Volume, error) {
	out, err := d.output(ctx, "volume", "inspect", "--format", "{{json .Labels}}", id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such volume") {
			return provider.Volume{}, fmt.Errorf("local-docker: %s: %w", id, provider.ErrVolumeNotFound)
		}
		return provider.Volume{}, err
	}
	l := map[string]string{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &l); err != nil {
		return provider.Volume{}, fmt.Errorf("local-docker: volume %s labels: %w", id, err)
	}
	users, err := d.volumeUsers(ctx, l[provider.LabelPool])
	if err != nil {
		return provider.Volume{}, err
	}
	return toVolume(id, l, users[id], d.readState()[id]), nil
}

// checkAttachable verifies that volume id exists and no machine uses it.
func (d *Docker) checkAttachable(ctx context.Context, id string) error {
	v, err := d.volume(ctx, id)
	if err != nil {
		return err
	}
	if v.Machine != "" {
		return fmt.Errorf("local-docker: volume %s: %w (machine %s)", id, provider.ErrVolumeInUse, v.Machine)
	}
	return d.updateState(func(st map[string]volState) { delete(st, id) })
}

// volumeUsers maps the volumes mounted by machine containers of owner
// at the cache directory to the machine.
func (d *Docker) volumeUsers(ctx context.Context, owner string) (map[string]string, error) {
	out, err := d.output(ctx, "ps", "-a", "--filter", "label="+provider.LabelPool+"="+owner,
		"--filter", "label="+provider.LabelKind+"=machine", "--format", "{{.Label \""+provider.LabelMachine+"\"}}\t{{.Mounts}}")
	if err != nil {
		return nil, err
	}
	users := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, mounts, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, m := range strings.Split(mounts, ",") {
			users[strings.TrimSpace(m)] = id
		}
	}
	return users, nil
}

func parseLabels(s string) map[string]string {
	l := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			l[k] = v
		}
	}
	return l
}

func toVolume(name string, l map[string]string, user string, st volState) provider.Volume {
	v := provider.Volume{ID: name, Zone: Zone, Labels: l, State: provider.VolumeAvailable}
	provider.VolumeFromLabels(&v)
	if v.Name == "" {
		v.Name = name
	}
	if st.ExpiresAt > 0 {
		v.ExpiresAt = time.Unix(st.ExpiresAt, 0)
	}
	if user != "" {
		v.State, v.Machine, v.ExpiresAt = provider.VolumeAttached, user, time.Time{}
	}
	return v
}

type volState struct {
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

func (d *Docker) statePath() string {
	dir := d.StateDir
	if dir == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			c = os.TempDir()
		}
		dir = filepath.Join(c, "offshore")
	}
	return filepath.Join(dir, "local-docker-volumes.json")
}

func (d *Docker) readState() map[string]volState {
	d.volMu.Lock()
	defer d.volMu.Unlock()
	return d.readStateLocked()
}

func (d *Docker) readStateLocked() map[string]volState {
	st := map[string]volState{}
	if b, err := os.ReadFile(d.statePath()); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (d *Docker) updateState(f func(map[string]volState)) error {
	d.volMu.Lock()
	defer d.volMu.Unlock()
	st := d.readStateLocked()
	f(st)
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	p := d.statePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
