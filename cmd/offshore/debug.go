package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/traumventures/offshore/internal/auth"
	"github.com/traumventures/offshore/internal/clock"
	"github.com/traumventures/offshore/internal/orchestrator"
)

// debugCmd holds operator tools around machine authentication:
//
//	offshore debug token --state-dir DIR --scope probe|session|admin --machine ID [--lease ID] [--ttl 5m]
//	offshore debug probe ADDR FINGERPRINT TOKEN
//	offshore debug clock [advance DURATION]
//
// "token" signs with the pool master key of a state dir (or
// $OFFSHORE_MASTER_KEY); "probe" prints a machine's status. "clock"
// shows or advances the test clock of $OFFSHORE_TEST_CLOCK
// (offset:FILE), which the orchestrator uses for idle stop and expiry:
// tests move time instead of waiting for it.
func debugCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: offshore debug token|probe|clock ...")
	}
	switch args[0] {
	case "clock":
		path := clock.OffsetFile()
		if path == "" {
			return fmt.Errorf("no test clock: set %s=offset:FILE for the daemon or control plane and this command", clock.EnvVar)
		}
		var off time.Duration
		var err error
		switch {
		case len(args) == 1:
			off, err = clock.ReadOffset(path)
		case len(args) == 3 && args[1] == "advance":
			var d time.Duration
			if d, err = time.ParseDuration(args[2]); err != nil {
				return err
			}
			if d < 0 {
				return errors.New("the test clock only moves forward")
			}
			off, err = clock.Advance(path, d)
		default:
			return errors.New("usage: offshore debug clock [advance DURATION]")
		}
		if err != nil {
			return err
		}
		fmt.Printf("offset %s, now %s\n", off, time.Now().Add(off).UTC().Format(time.RFC3339))
		return nil
	case "token":
		fs := newFlagSet("debug token")
		dir := fs.String("state-dir", "", "orchestrator state directory holding master.key")
		scope := fs.String("scope", string(auth.ScopeProbe), "probe, session or admin")
		machine := fs.String("machine", "", "machine id")
		lease := fs.String("lease", "", "lease id (session tokens)")
		ttl := fs.Duration("ttl", 5*time.Minute, "validity")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" && os.Getenv(auth.EnvMasterKey) == "" && os.Getenv(auth.EnvMasterKeyFile) == "" {
			return errors.New("--state-dir or $OFFSHORE_MASTER_KEY is required")
		}
		if *dir != "" {
			// Never create a key here: a token from a new key is useless.
			if _, err := os.Stat(filepath.Join(*dir, auth.MasterKeyFile)); err != nil {
				return err
			}
		}
		m, err := auth.LoadMaster(*dir)
		if err != nil {
			return err
		}
		fmt.Println(auth.NewToken(m.Signer(), auth.Scope(*scope), *machine, *lease, time.Now().Add(*ttl)))
		return nil
	case "probe":
		if len(args) != 4 {
			return errors.New("usage: offshore debug probe ADDR FINGERPRINT TOKEN")
		}
		// A diagnostic command: wait longer than the orchestrator would.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, err := orchestrator.Probe(ctx, args[1], args[2], args[3])
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("unknown debug command %q", args[0])
}
