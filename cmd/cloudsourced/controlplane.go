package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/traumventures/cloudsourced/internal/client"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/fleet"
	"github.com/traumventures/cloudsourced/internal/transport"
)

// controlplane serves the shared fleet API. Without --tls-cert it uses a
// persistent self-signed certificate whose fingerprint clients pin, so no
// domain is needed; with a domain, pass a certificate or terminate TLS in a
// reverse proxy and use --insecure-http behind it.
func controlplane(args []string) error {
	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	listen := fs.String("listen", ":7443", "listen address")
	stateDir := fs.String("state-dir", "", "state directory (default $CLOUDSOURCED_STATE_DIR or <state>/controlplane)")
	certFile := fs.String("tls-cert", "", "TLS certificate (PEM); default: self-signed, pinned by fingerprint")
	keyFile := fs.String("tls-key", "", "TLS private key (PEM)")
	plain := fs.Bool("insecure-http", false, "serve plain HTTP (only behind a TLS terminating proxy)")
	keyPath := fs.String("api-key-file", "", "file with the API key (default $CLOUDSOURCED_API_KEY or a generated <state>/api-key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *stateDir
	if dir == "" {
		if dir = os.Getenv("CLOUDSOURCED_STATE_DIR"); dir == "" {
			dir = filepath.Join(client.StateDir(), "controlplane")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log := logger()
	u, err := config.LoadUser()
	if err != nil {
		return err
	}
	apiKey, generated, err := loadAPIKey(*keyPath, dir)
	if err != nil {
		return err
	}
	local, err := fleet.NewLocal(u, dir, log)
	if err != nil {
		return err
	}
	srv, err := fleet.NewServer(local, apiKey, dir, log)
	if err != nil {
		return err
	}
	srv.Reload = config.LoadUser
	hs := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go local.Run(ctx, 15*time.Second)
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		hs.Shutdown(sctx)
	}()

	fmt.Fprintf(os.Stderr, "cloudsourced control plane %s\n  listen:      %s\n  config:      %s\n  state:       %s\n  providers:   %s\n",
		version, *listen, config.UserConfigPath(), dir, strings.Join(u.ProviderNames(), ", "))
	if generated {
		fmt.Fprintf(os.Stderr, "  api key:     %s (generated, stored in %s)\n", apiKey, filepath.Join(dir, "api-key"))
	}
	switch {
	case *plain:
		fmt.Fprintln(os.Stderr, "  tls:         off (expects a TLS terminating proxy)")
		err = hs.ListenAndServe()
	case *certFile != "":
		fmt.Fprintln(os.Stderr, "  tls:         "+*certFile)
		err = hs.ListenAndServeTLS(*certFile, *keyFile)
	default:
		id, ierr := selfSigned(dir)
		if ierr != nil {
			return ierr
		}
		cert, cerr := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
		if cerr != nil {
			return cerr
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		fmt.Fprintf(os.Stderr, "  tls:         self-signed, fingerprint %s\n", id.Fingerprint)
		err = hs.ListenAndServeTLS("", "")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loadAPIKey(path, dir string) (key string, generated bool, err error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", false, err
		}
		key = strings.TrimSpace(string(b))
	} else if key = os.Getenv("CLOUDSOURCED_API_KEY"); key == "" {
		p := filepath.Join(dir, "api-key")
		if b, err := os.ReadFile(p); err == nil {
			key = strings.TrimSpace(string(b))
		} else {
			buf := make([]byte, 32)
			rand.Read(buf)
			key = "cs_" + hex.EncodeToString(buf)
			if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
				return "", false, err
			}
			generated = true
		}
	}
	if len(key) < 16 {
		return "", false, errors.New("API key must have at least 16 characters")
	}
	return key, generated, nil
}

// selfSigned returns the persistent control plane certificate, so that its
// fingerprint survives restarts.
func selfSigned(dir string) (*transport.Identity, error) {
	p := filepath.Join(dir, "tls.json")
	if b, err := os.ReadFile(p); err == nil {
		var id transport.Identity
		if json.Unmarshal(b, &id) == nil && id.Fingerprint != "" {
			return &id, nil
		}
	}
	id, err := transport.NewIdentity("cloudsourced-controlplane", 10*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(id)
	return id, os.WriteFile(p, b, 0o600)
}
