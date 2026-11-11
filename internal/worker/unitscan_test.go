package worker

import (
	"reflect"
	"testing"
)

func TestUnitScanner(t *testing.T) {
	u := &unitScanner{}
	out := "\x1b[1m\x1b[32m   Compiling\x1b[0m proc-macro2 v1.0.107\n" +
		"    Checking app-kernel v0.0.0 (/w/app/crates/foundation/app-kernel)\r" +
		"    Building [=====>    ] 10/20: app-api\r" +
		"   0.402s  INFO prepare_target{force=false package_id=app-api v0.0.0 target=\"app_api\"}: cargo::core::compiler::fingerprint: dirty: FsStatusOutdated(StaleItem(ChangedFile { reference: \"x\" }))\n" +
		"   0.5s  INFO prepare_target{}: cargo::core::compiler::fingerprint: dirty: UnitDependencyInfoChanged { name: \"a\" }\n" +
		"warning: unused\n" +
		"    Finished `dev` profile [unoptimized + debuginfo] target(s) in 3.46s\n" +
		"  Documenting app-api"
	// Written in odd chunks: lines span writes.
	for i := 0; i < len(out); i += 7 {
		u.Write([]byte(out[i:min(len(out), i+7)]))
	}
	n, sample, reasons := u.result()
	if n != 3 || !reflect.DeepEqual(sample, []string{"proc-macro2", "app-kernel", "app-api"}) {
		t.Fatalf("units %d %v", n, sample)
	}
	want := map[string]int{"FsStatusOutdated/StaleItem": 1, "UnitDependencyInfoChanged": 1}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("reasons %v", reasons)
	}
}
