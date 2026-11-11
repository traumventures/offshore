package main

import (
	"bytes"
	"strings"
	"testing"
)

// Every subcommand dispatched by run has a help text.
func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range []string{"run", "worker", "supervisor", "gocacheprog", "sandbox-init", "controlplane", "sim", "report",
		"daemon", "status", "top", "provider", "prewarm", "hooks", "why", "metrics", "down", "reconcile", "shim", "fetch", "debug",
		"env", "init", "doctor", "editor", "outputs", "config", "version", "help"} {
		if commandHelp[c] == "" {
			t.Errorf("%s: no help text", c)
		}
	}
}

func TestHelpRequest(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
		want bool
	}{
		{"editor", []string{"--help"}, true},
		{"status", []string{"-h"}, true},
		{"fetch", []string{"target/*", "-h"}, true},
		{"run", []string{"--help"}, true},
		{"run", []string{"cargo", "--help"}, false}, // the tool's help
		{"fetch", []string{"--", "-h"}, false},
		{"top", []string{"-h"}, false},             // FlagSet prints usage and defaults
		{"config", []string{"check", "-h"}, false}, // FlagSet
		{"config", []string{"-h"}, true},
		{"debug", []string{"-h"}, true},
		{"debug", []string{"token", "-h"}, false},
		{"gocacheprog", []string{"--help"}, true},
		{"status", nil, false},
	}
	for _, c := range cases {
		if _, got := helpRequest(c.cmd, c.args); got != c.want {
			t.Errorf("%s %v: help = %v, want %v", c.cmd, c.args, got, c.want)
		}
	}
}

func TestHelpListsAllCommands(t *testing.T) {
	var b bytes.Buffer
	if err := printHelp(&b, ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"offshore gocacheprog", "offshore debug", "offshore editor", "offshore help"} {
		if !strings.Contains(b.String(), c) {
			t.Errorf("help lacks %q", c)
		}
	}
	if err := printHelp(&b, "nope"); err == nil {
		t.Error("unknown command accepted")
	}
}

// run returns help instead of executing the command.
func TestRunHelp(t *testing.T) {
	if err := run("editor", []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if err := run("down", []string{"-h"}); err != nil {
		t.Fatal(err) // must not contact the daemon
	}
}
