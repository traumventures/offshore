package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/config"
)

func linkTo(t *testing.T, target, link string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func fakeBinary(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCheckShimsOrder(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "bin", "offshore")
	fakeBinary(t, self)
	shims := filepath.Join(dir, "shims")
	linkTo(t, self, filepath.Join(shims, "cargo"))
	sys := filepath.Join(dir, "usr")
	fakeBinary(t, filepath.Join(sys, "cargo"))

	if c := checkShims(shims, shims+":"+sys, self); c.Status != checkOK {
		t.Errorf("shims first: %+v", c)
	}
	c := checkShims(shims, sys+":"+shims, self)
	if c.Status != checkFail || !strings.Contains(c.Detail, "cargo resolves to "+filepath.Join(sys, "cargo")) || !strings.Contains(c.Hint, shims) {
		t.Errorf("real cargo first: %+v", c)
	}
	if c := checkShims(shims, sys, self); c.Status != checkFail {
		t.Errorf("shim dir not in PATH: %+v", c)
	}
	if c := checkShims(filepath.Join(dir, "none"), sys, self); c.Status != checkWarn {
		t.Errorf("no shims: %+v", c)
	}
}

func TestCheckBinary(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "a", "offshore")
	fakeBinary(t, self)
	other := filepath.Join(dir, "b", "offshore")
	fakeBinary(t, other)
	if c := checkBinary(self, filepath.Dir(self)); c.Status != checkOK {
		t.Errorf("%+v", c)
	}
	if c := checkBinary(self, filepath.Dir(other)+":"+filepath.Dir(self)); c.Status != checkWarn || !strings.Contains(c.Detail, other) {
		t.Errorf("%+v", c)
	}
	if c := checkBinary(self, filepath.Join(dir, "empty")); c.Status != checkWarn {
		t.Errorf("%+v", c)
	}
}

func TestCheckClock(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for skew, want := range map[time.Duration]string{0: checkOK, 3 * time.Second: checkOK, -20 * time.Second: checkWarn, 5 * time.Minute: checkFail} {
		if c := checkClock(now, now.Add(skew)); c.Status != want {
			t.Errorf("skew %s: %+v", skew, c)
		}
	}
	if c := checkClock(time.Time{}, now); c.Status != checkSkip {
		t.Errorf("%+v", c)
	}
}

func TestCheckFuse(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "fuse")
	os.WriteFile(dev, nil, 0o600)
	found := func(string) (string, error) { return "/usr/bin/fusermount3", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if c := checkFuse(dev, found, true); c.Status != checkOK {
		t.Errorf("%+v", c)
	}
	if c := checkFuse(filepath.Join(dir, "nope"), found, false); c.Status != checkWarn {
		t.Errorf("not needed: %+v", c)
	}
	if c := checkFuse(dev, missing, true); c.Status != checkFail || !strings.Contains(c.Detail, "fusermount3") {
		t.Errorf("needed: %+v", c)
	}
}

func TestCheckSecretsNeverPrintsValues(t *testing.T) {
	t.Setenv("DOCTOR_SECRET", "s3cr3t-value")
	repo := &config.Repo{Root: t.TempDir(), Secrets: map[string]string{
		"A": "env:DOCTOR_SECRET", "B": "cmd:printf hidden-%s token", "C": "cmd:echo nope >&2; exit 3", "D": "env:DOCTOR_UNSET_VAR",
	}}
	cs := checkSecrets(repo)
	got := map[string]string{}
	all, _ := json.Marshal(cs)
	for _, c := range cs {
		got[c.Name] = c.Status
	}
	want := map[string]string{"secret A": checkOK, "secret B": checkOK, "secret C": checkFail, "secret D": checkFail}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s (%s)", k, got[k], v, all)
		}
	}
	if strings.Contains(string(all), "s3cr3t-value") || strings.Contains(string(all), "hidden-token") {
		t.Fatalf("secret value printed: %s", all)
	}
	if !strings.Contains(string(all), "nope") {
		t.Errorf("command error not explained: %s", all)
	}
}

func TestPrintDoctorExitCode(t *testing.T) {
	var buf bytes.Buffer
	ok := []check{{Name: "a", Status: checkOK}, {Name: "b", Status: checkWarn, Hint: "h"}}
	if err := printDoctor(&buf, ok, false); err != nil || !strings.Contains(buf.String(), "WARN") {
		t.Fatalf("%v %q", err, buf.String())
	}
	buf.Reset()
	err := printDoctor(&buf, append(ok, check{Name: "c", Status: checkFail}), true)
	var ec exitCode
	if !errors.As(err, &ec) || ec != 1 {
		t.Fatalf("want exit code 1, got %v", err)
	}
	var rep doctorReport
	if json.Unmarshal(buf.Bytes(), &rep) != nil || rep.OK || len(rep.Checks) != 3 {
		t.Fatalf("json %s", buf.String())
	}
}

// controlPlane serves /healthz and /v1/status like the real one.
func controlPlane(t *testing.T, key string, date time.Time) (*httptest.Server, string) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", date.UTC().Format(http.TimeFormat))
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte("ok\n"))
		case "/v1/status":
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"done":true,"result":[]}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, hex.EncodeToString(sum[:])
}

func clientFor(t *testing.T, url, fp, keyRef string) *config.Client {
	return &config.Client{Path: filepath.Join(t.TempDir(), "config.toml"),
		ControlPlane: config.ControlPlane{Mode: "remote", URL: url, Fingerprint: fp, APIKey: keyRef}}
}

func statuses(cs []check) map[string]string {
	m := map[string]string{}
	for _, c := range cs {
		m[c.Name] = c.Status
	}
	return m
}

func TestCheckControlPlane(t *testing.T) {
	now := time.Now()
	srv, fp := controlPlane(t, "k1", now.Add(-2*time.Minute))
	t.Setenv("DOCTOR_KEY", "k1")
	ctx := context.Background()
	clock := func() time.Time { return now }

	cs, r := checkControlPlane(ctx, clientFor(t, srv.URL, fp, "env:DOCTOR_KEY"), clock)
	if s := statuses(cs); s["control plane"] != checkOK || s["api key"] != checkOK || s["clock"] != checkFail || r == nil {
		t.Fatalf("good setup: %+v", cs)
	}
	cs, r = checkControlPlane(ctx, clientFor(t, srv.URL, strings.Repeat("0", 64), "env:DOCTOR_KEY"), clock)
	if s := statuses(cs); s["control plane"] != checkFail || r != nil || !strings.Contains(cs[0].Detail, "fingerprint") {
		t.Fatalf("wrong fingerprint: %+v", cs)
	}
	t.Setenv("DOCTOR_KEY", "wrong")
	cs, r = checkControlPlane(ctx, clientFor(t, srv.URL, fp, "env:DOCTOR_KEY"), clock)
	if s := statuses(cs); s["control plane"] != checkOK || s["api key"] != checkFail || r != nil {
		t.Fatalf("wrong key: %+v", cs)
	}
	cs, _ = checkControlPlane(ctx, clientFor(t, srv.URL, fp, "env:DOCTOR_MISSING_KEY"), clock)
	if s := statuses(cs); s["control plane"] != checkFail || !strings.Contains(cs[0].Detail, "DOCTOR_MISSING_KEY") {
		t.Fatalf("missing key: %+v", cs)
	}
	srv.Close()
	cs, _ = checkControlPlane(ctx, clientFor(t, srv.URL, fp, "env:DOCTOR_KEY"), clock)
	if s := statuses(cs); s["control plane"] != checkFail || s["clock"] != checkSkip {
		t.Fatalf("down: %+v", cs)
	}
}
