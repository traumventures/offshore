package adapter

import (
	"testing"

	"github.com/traumventures/offshore/internal/access"
)

func TestAccessPriors(t *testing.T) {
	m := access.New(AccessPriors()...)
	want := map[string]access.Class{
		"target/debug/deps/libserde-0123456789abcdef.rlib":                        access.Immutable,
		"target/x86_64-unknown-linux-gnu/release/deps/libx-0123456789abcdef.rlib": access.Immutable,
		"target/debug/incremental/app-0123456789abcdef/s-hq0001zz-working/a.bin":  access.Churn,
		"crates/a/target/debug/.fingerprint/app-0123456789abcdef/bin-app":         access.Churn,
		"target/debug/build/ring-0123456789abcdef/out/libring.a":                  access.WriteOnce,
		"target/debug/build/ring-0123456789abcdef/build-script-build":             access.WriteOnce,
		"target/tmp/x": access.Ephemeral,
		"home/.cache/go-build/ab/abcdef0123456789-d": access.WriteOnce,
		"home/.cache/go-build/trim.txt":              access.Churn,
		"go/pkg/mod/github.com/x/y@v1.0.0/a.go":      access.Immutable,
		"web/node_modules/react/index.js":            access.Immutable,
		"web/node_modules/.cache/babel/x.json":       access.Churn,
		"web/.next/cache/webpack/a.pack":             access.Churn,
		"src/main.rs":                                access.Unknown,
		"target/debug/app":                           access.Unknown,
	}
	for p, c := range want {
		if d := m.Classify(p); d.Class != c {
			t.Errorf("%s: %+v want %v", p, d, c)
		}
	}
	if _, ok := Adapter(Generic{}).(AccessHinter); ok {
		t.Error("generic has no priors")
	}
}
