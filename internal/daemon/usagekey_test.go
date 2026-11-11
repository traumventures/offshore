package daemon

import "testing"

func TestUsageKey(t *testing.T) {
	for _, c := range []struct {
		tool string
		args []string
		want string
	}{
		{"cargo", []string{"check"}, "r/cargo/check"},
		{"cargo", []string{"check", "-p", "app-api"}, "r/cargo/check?p=app-api"},
		{"cargo", []string{"+nightly", "clippy", "--package=b", "-pa", "--release"}, "r/cargo/clippy/release?p=a,b"},
		{"cargo", []string{"clippy", "--workspace", "--all-targets", "--", "-D", "warnings"}, "r/cargo/clippy?workspace"},
		{"cargo", []string{"nextest", "archive", "--archive-file", "x"}, "r/cargo/nextest"},
		{"", nil, "r/offshore/fetch"},
	} {
		if got := UsageKey("r", c.tool, c.args); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.tool, c.args, got, c.want)
		}
	}
}
