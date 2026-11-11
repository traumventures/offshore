package config

import (
	"strings"
	"testing"
)

func TestStorageSection(t *testing.T) {
	f, err := loadUserFrom(t, `
[pool]
id = "team"
[storage]
endpoint = "https://acc.r2.cloudflarestorage.com"
bucket = "builds"
access_key = "env:R2_KEY"
secret_key = "env:R2_SECRET"
`)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Storage.Configured() || f.Storage.Prefix != "cache/team" || f.Storage.Bucket != "builds" {
		t.Fatalf("storage %+v", f.Storage)
	}
	doc, err := f.Document()
	if err != nil || doc["storage"] == nil {
		t.Fatalf("document %v %v", doc, err)
	}
	for body, want := range map[string]string{
		"[storage]\nregion = \"x\"\n":                                             "bucket (or dir) required",
		"[storage]\nbucket = \"b\"\n":                                             "role_arn or access_key",
		"[storage]\nbucket = \"b\"\naccess_key = \"env:A\"\n":                     "go together",
		"[storage]\nbucket = \"b\"\nrole_arn = \"r\"\ndir = \"/s\"\n":             "exclusive",
		"[storage]\nbucket = \"b\"\naccess_key = \"A\"\nsecret_key = \"env:S\"\n": "storage.access_key",
	} {
		if _, err := loadUserFrom(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
	if f, err := loadUserFrom(t, "[storage]\ndir = \"/srv/store\"\n"); err != nil || f.Storage.Dir != "/srv/store" {
		t.Fatalf("dir store: %v", err)
	}
}
