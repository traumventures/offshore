package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/traumventures/cloudsourced/internal/auth"
	"github.com/traumventures/cloudsourced/internal/orchestrator"
)

// debugCmd holds operator tools around machine authentication:
//
//	cloudsourced debug token --state-dir DIR --scope probe|session|admin --machine ID [--lease ID] [--ttl 5m]
//	cloudsourced debug probe ADDR FINGERPRINT TOKEN
//
// "token" signs with the fleet master key of a state dir (or
// $CLOUDSOURCED_MASTER_KEY); "probe" prints a machine's status.
func debugCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cloudsourced debug token|probe ...")
	}
	switch args[0] {
	case "token":
		fs := flag.NewFlagSet("debug token", flag.ContinueOnError)
		dir := fs.String("state-dir", "", "orchestrator state directory holding master.key")
		scope := fs.String("scope", string(auth.ScopeProbe), "probe, session or admin")
		machine := fs.String("machine", "", "machine id")
		lease := fs.String("lease", "", "lease id (session tokens)")
		ttl := fs.Duration("ttl", 5*time.Minute, "validity")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" && os.Getenv(auth.EnvMasterKey) == "" && os.Getenv(auth.EnvMasterKeyFile) == "" {
			return errors.New("--state-dir or $CLOUDSOURCED_MASTER_KEY is required")
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
			return errors.New("usage: cloudsourced debug probe ADDR FINGERPRINT TOKEN")
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
