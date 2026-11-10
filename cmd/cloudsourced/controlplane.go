package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/traumventures/cloudsourced/internal/client"
	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/fleet"
	"github.com/traumventures/cloudsourced/internal/transport"
)

const (
	defaultListenSelfSigned = ":7443"
	defaultListenACME       = ":443"
	defaultACMEHTTP         = ":80"
	cpStateFile             = "controlplane.json"
	cpCertCN                = "cloudsourced-controlplane"
)

// cpOptions are the flags of "cloudsourced controlplane".
type cpOptions struct {
	Listen        string // resolved: never empty after parsing
	StateDir      string // resolved
	CertFile      string
	KeyFile       string
	Plain         bool
	APIKeyFile    string
	Hostname      string
	ACME          bool
	ACMEEmail     string
	ACMEDirectory string
	ACMEHTTP      string // listen address for HTTP-01 challenges ("" = off)
	MachineBinary string // "" auto, "self", or a path on the Docker host
	ClientCA      string // require client certificates signed by this CA (mutual TLS)
	MetricsListen string // plain HTTP /metrics without authentication ("" = off)
}

// cpState is what a running control plane publishes for "controlplane
// info", "client-config" and "health". It holds no secrets.
type cpState struct {
	Version     string    `json:"version"`
	Listen      string    `json:"listen"`
	Hostname    string    `json:"hostname,omitempty"`
	TLS         string    `json:"tls"` // self-signed | acme | file | off
	Fingerprint string    `json:"fingerprint,omitempty"`
	URLs        []string  `json:"urls"`
	APIKey      string    `json:"api_key"` // where the key comes from, never the key
	Started     time.Time `json:"started"`
}

func controlplaneUsage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprint(fs.Output(), `usage:
  cloudsourced controlplane [flags]                  serve the fleet API
  cloudsourced controlplane info [--show-key]        URLs, fingerprint, API key location
  cloudsourced controlplane client-config [--url U]  print the client [orchestrator] block
  cloudsourced controlplane health                   check a running control plane (exit status)

Without --hostname the control plane uses a persistent self-signed certificate
that clients pin by fingerprint. With --hostname cs.example.com (a DNS name
pointing at this host) it obtains a publicly trusted certificate from Let's
Encrypt (ACME; port 80 or the API port 443 must be reachable from the
internet) and clients only set url = "https://cs.example.com".

flags:
`)
		fs.PrintDefaults()
	}
}

// parseControlplaneFlags parses and resolves the serve flags.
func parseControlplaneFlags(args []string) (*cpOptions, error) {
	o := &cpOptions{}
	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	fs.StringVar(&o.Listen, "listen", os.Getenv("CLOUDSOURCED_LISTEN"), "listen address (default "+defaultListenSelfSigned+", "+defaultListenACME+" with ACME; $CLOUDSOURCED_LISTEN)")
	fs.StringVar(&o.StateDir, "state-dir", "", "state directory (default $CLOUDSOURCED_STATE_DIR or <state>/controlplane)")
	fs.StringVar(&o.Hostname, "hostname", os.Getenv("CLOUDSOURCED_HOSTNAME"), "public DNS name of the control plane; enables ACME unless --acme=false, where it becomes a SAN of the self-signed certificate ($CLOUDSOURCED_HOSTNAME)")
	acmeFlag := fs.Bool("acme", false, "obtain a certificate via ACME (Let's Encrypt) for --hostname (default true when --hostname is a DNS name)")
	fs.StringVar(&o.ACMEEmail, "acme-email", os.Getenv("CLOUDSOURCED_ACME_EMAIL"), "contact e-mail for the ACME account ($CLOUDSOURCED_ACME_EMAIL)")
	fs.StringVar(&o.ACMEDirectory, "acme-directory", os.Getenv("CLOUDSOURCED_ACME_DIRECTORY"), "ACME directory URL (default Let's Encrypt production; $CLOUDSOURCED_ACME_DIRECTORY)")
	fs.StringVar(&o.ACMEHTTP, "acme-http", "", "listen address for ACME HTTP-01 challenges, \"off\" to rely on TLS-ALPN-01 on the API port (default "+defaultACMEHTTP+" with ACME)")
	fs.StringVar(&o.CertFile, "tls-cert", "", "TLS certificate (PEM) instead of self-signed or ACME")
	fs.StringVar(&o.KeyFile, "tls-key", "", "TLS private key (PEM)")
	fs.BoolVar(&o.Plain, "insecure-http", false, "serve plain HTTP (only behind a TLS terminating proxy)")
	fs.StringVar(&o.ClientCA, "client-ca", "", "require client certificates signed by this CA (PEM) in addition to the API key (mutual TLS)")
	fs.StringVar(&o.APIKeyFile, "api-key-file", "", "file with the API key (default $CLOUDSOURCED_API_KEY or a generated <state>/api-key)")
	fs.StringVar(&o.MachineBinary, "machine-binary", os.Getenv("CLOUDSOURCED_MACHINE_BINARY"), "server binary of local-docker machines as a path on the Docker host; \"self\" = this executable; default: this executable, or a copy in <state>/bin when running in a container ($CLOUDSOURCED_MACHINE_BINARY)")
	fs.StringVar(&o.MetricsListen, "metrics-listen", os.Getenv("CLOUDSOURCED_METRICS_LISTEN"), "also serve GET /metrics over plain HTTP without authentication on this address, e.g. 127.0.0.1:9090 (/metrics on the API port needs the API key; $CLOUDSOURCED_METRICS_LISTEN)")
	fs.Usage = controlplaneUsage(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("controlplane: unexpected argument %q", fs.Arg(0))
	}
	acmeSet := false
	fs.Visit(func(f *flag.Flag) { acmeSet = acmeSet || f.Name == "acme" })
	o.Hostname = strings.TrimSuffix(strings.TrimSpace(o.Hostname), ".")
	o.ACME = *acmeFlag
	if !acmeSet {
		// The hostname alone switches to a publicly trusted certificate.
		o.ACME = o.Hostname != "" && net.ParseIP(o.Hostname) == nil && o.CertFile == "" && !o.Plain
	}
	if o.ACME {
		switch {
		case o.Hostname == "":
			return nil, errors.New("controlplane: --acme needs --hostname")
		case net.ParseIP(o.Hostname) != nil:
			return nil, fmt.Errorf("controlplane: ACME needs a DNS name, not the IP address %s (use --acme=false)", o.Hostname)
		case !strings.Contains(o.Hostname, "."):
			return nil, fmt.Errorf("controlplane: ACME needs a fully qualified DNS name, have %q (use --acme=false)", o.Hostname)
		case o.CertFile != "" || o.Plain:
			return nil, errors.New("controlplane: --acme cannot be combined with --tls-cert or --insecure-http")
		}
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, errors.New("controlplane: --tls-cert and --tls-key go together")
	}
	if o.CertFile != "" && o.Plain {
		return nil, errors.New("controlplane: --tls-cert and --insecure-http exclude each other")
	}
	if o.ClientCA != "" && o.Plain {
		return nil, errors.New("controlplane: --client-ca needs TLS (drop --insecure-http)")
	}
	if o.Listen == "" {
		o.Listen = defaultListenSelfSigned
		if o.ACME {
			o.Listen = defaultListenACME
		}
	}
	switch {
	case !o.ACME:
		o.ACMEHTTP = ""
	case o.ACMEHTTP == "":
		o.ACMEHTTP = defaultACMEHTTP
	case o.ACMEHTTP == "off":
		o.ACMEHTTP = ""
	}
	o.StateDir = cpStateDir(o.StateDir)
	return o, nil
}

func cpStateDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if d := os.Getenv("CLOUDSOURCED_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(client.StateDir(), "controlplane")
}

// controlplane serves the shared fleet API.
func controlplane(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "info":
			return controlplaneInfo(args[1:])
		case "client-config":
			return controlplaneClientConfig(args[1:])
		case "health":
			return controlplaneHealth(args[1:])
		case "serve":
			args = args[1:]
		}
	}
	o, err := parseControlplaneFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	return serveControlplane(o)
}

func serveControlplane(o *cpOptions) error {
	dir := o.StateDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	log := logger()
	apiKey, keySource, generated, err := loadAPIKey(o.APIKeyFile, dir)
	if err != nil {
		return err
	}
	u, err := config.LoadUser()
	if err != nil {
		return err
	}
	// Only local-docker mounts the binary from the Docker host; other
	// providers upload it themselves.
	bin := ""
	if hasLocalDocker(u) {
		if bin, err = resolveMachineBinary(o.MachineBinary, dir, func(msg string, args ...any) { log.Warn(msg, args...) }); err != nil {
			return err
		}
	}
	setMachineBinary(u, bin)
	load := func() (*config.User, error) {
		u, err := config.LoadUser()
		if err == nil {
			setMachineBinary(u, bin)
		}
		return u, err
	}
	tc, mgr, st, err := cpTLS(o, dir)
	if err != nil {
		return err
	}
	if o.ClientCA != "" {
		pem, err := os.ReadFile(o.ClientCA)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("--client-ca %s: no certificates found", o.ClientCA)
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
		log.Info("client auth: mutual TLS", "ca", o.ClientCA)
	}
	local, err := fleet.NewLocal(u, dir, log)
	if err != nil {
		return err
	}
	srv, err := fleet.NewServer(local, apiKey, dir, log)
	if err != nil {
		return err
	}
	srv.Reload = load
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second, TLSConfig: tc}
	var metricsSrv *http.Server
	if o.MetricsListen != "" {
		if host, _, _ := net.SplitHostPort(o.MetricsListen); host == "" || !(host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			log.Warn("--metrics-listen is not a loopback address; /metrics there has no authentication", "addr", o.MetricsListen)
		}
		metricsSrv = &http.Server{Addr: o.MetricsListen, Handler: srv.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
	}
	var challenge *http.Server
	if mgr != nil && o.ACMEHTTP != "" {
		// HTTP-01 challenges; every other request is redirected to HTTPS.
		challenge = &http.Server{Addr: o.ACMEHTTP, Handler: mgr.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}
	}
	// Signals are handled from here on; the state file below tells
	// observers the control plane is up.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	st.Version, st.Listen, st.Hostname, st.Started = version, ln.Addr().String(), o.Hostname, time.Now().UTC()
	st.APIKey = keySource
	st.URLs = suggestURLs(o.Hostname, st.TLS, ln.Addr().String())
	if b, err := json.MarshalIndent(st, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dir, cpStateFile), append(b, '\n'), 0o644)
	}

	go local.Run(ctx, 15*time.Second)

	fmt.Fprintf(os.Stderr, "cloudsourced control plane %s\n  listen:      %s\n  config:      %s\n  state:       %s\n  providers:   %s\n",
		version, st.Listen, config.UserConfigPath(), dir, strings.Join(u.ProviderNames(), ", "))
	if bin != "" {
		fmt.Fprintf(os.Stderr, "  machine bin: %s (path on the Docker host)\n", bin)
	}
	switch st.TLS {
	case "off":
		fmt.Fprintln(os.Stderr, "  tls:         off (expects a TLS terminating proxy)")
	case "file":
		fmt.Fprintln(os.Stderr, "  tls:         "+o.CertFile)
	case "acme":
		fmt.Fprintf(os.Stderr, "  tls:         ACME certificate for %s (cache %s)\n", o.Hostname, filepath.Join(dir, "acme"))
		if challenge != nil {
			fmt.Fprintf(os.Stderr, "  acme http:   %s (HTTP-01 challenges, redirects to https)\n", o.ACMEHTTP)
		}
	default:
		fmt.Fprintf(os.Stderr, "  tls:         self-signed, fingerprint %s\n", st.Fingerprint)
	}
	if metricsSrv != nil {
		fmt.Fprintf(os.Stderr, "  metrics:     http://%s/metrics (no authentication)\n", o.MetricsListen)
	}
	fmt.Fprintf(os.Stderr, "  api key:     %s", keySource)
	if generated {
		fmt.Fprint(os.Stderr, " (generated)")
	}
	fmt.Fprintf(os.Stderr, "\n  urls:        %s\n  clients:     cloudsourced controlplane client-config --state-dir %s\n",
		strings.Join(st.URLs, " "), dir)

	errc := make(chan error, 2)
	go func() {
		if st.TLS == "off" {
			errc <- hs.Serve(ln)
		} else {
			errc <- hs.ServeTLS(ln, "", "")
		}
	}()
	if challenge != nil {
		go func() {
			if err := challenge.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("acme http listener: %w", err)
			}
		}()
	}
	if metricsSrv != nil {
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("metrics listener: %w", err)
			}
		}()
	}
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	cancel()
	log.Info("control plane shutting down")
	sctx, c := context.WithTimeout(context.Background(), 20*time.Second)
	defer c()
	if challenge != nil {
		challenge.Shutdown(sctx)
	}
	if metricsSrv != nil {
		metricsSrv.Shutdown(sctx)
	}
	if serr := hs.Shutdown(sctx); serr != nil && err == nil {
		err = serr
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// cpTLS selects the certificate: a given file, ACME for --hostname, plain
// HTTP, or the persistent self-signed certificate (--hostname as SAN).
func cpTLS(o *cpOptions, dir string) (*tls.Config, *autocert.Manager, *cpState, error) {
	st := &cpState{}
	switch {
	case o.Plain:
		st.TLS = "off"
		return nil, nil, st, nil
	case o.CertFile != "":
		cert, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, nil, nil, err
		}
		st.TLS = "file"
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil, st, nil
	case o.ACME:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(filepath.Join(dir, "acme")),
			HostPolicy: autocert.HostWhitelist(o.Hostname),
			Email:      o.ACMEEmail,
		}
		if o.ACMEDirectory != "" {
			m.Client = &acme.Client{DirectoryURL: o.ACMEDirectory}
		}
		tc := m.TLSConfig() // also answers TLS-ALPN-01 (acme-tls/1)
		tc.MinVersion = tls.VersionTLS12
		st.TLS = "acme"
		return tc, m, st, nil
	}
	id, err := selfSigned(dir, o.Hostname)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := tls.X509KeyPair([]byte(id.CertPEM), []byte(id.KeyPEM))
	if err != nil {
		return nil, nil, nil, err
	}
	st.TLS, st.Fingerprint = "self-signed", id.Fingerprint
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil, st, nil
}

// suggestURLs lists URLs clients may use, the hostname first.
func suggestURLs(hostname, mode, addr string) []string {
	scheme := "https"
	if mode == "off" {
		scheme = "http"
	}
	host, port, _ := net.SplitHostPort(addr)
	url := func(h string) string {
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			return scheme + "://" + h
		}
		return scheme + "://" + net.JoinHostPort(h, port)
	}
	var out []string
	if hostname != "" {
		out = append(out, url(hostname))
	}
	if mode == "acme" {
		return out // the certificate is only valid for the hostname
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		if host != hostname {
			out = append(out, url(host))
		}
		return out
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() && n.IP.String() != hostname {
			out = append(out, url(n.IP.String()))
		}
	}
	return out
}

// loadAPIKey returns the API key and where it comes from (never logged).
func loadAPIKey(path, dir string) (key, source string, generated bool, err error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", "", false, err
		}
		key, source = strings.TrimSpace(string(b)), path
	} else if key = os.Getenv("CLOUDSOURCED_API_KEY"); key != "" {
		source = "$CLOUDSOURCED_API_KEY"
	} else {
		p := filepath.Join(dir, "api-key")
		source = p
		if b, err := os.ReadFile(p); err == nil {
			key = strings.TrimSpace(string(b))
		} else {
			buf := make([]byte, 32)
			rand.Read(buf)
			key = "cs_" + hex.EncodeToString(buf)
			if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
				return "", "", false, err
			}
			generated = true
		}
	}
	if len(key) < 16 {
		return "", "", false, errors.New("API key must have at least 16 characters")
	}
	return key, source, generated, nil
}

// selfSigned returns the persistent control plane certificate, so that its
// fingerprint survives restarts. A hostname (DNS name or IP) becomes a
// subject alternative name; the certificate is only replaced (and the
// fingerprint changes) when it does not cover that name yet.
func selfSigned(dir, hostname string) (*transport.Identity, error) {
	p := filepath.Join(dir, "tls.json")
	if b, err := os.ReadFile(p); err == nil {
		var id transport.Identity
		if json.Unmarshal(b, &id) == nil && id.Fingerprint != "" {
			if hostname == "" || certCovers(id.CertPEM, hostname) {
				return &id, nil
			}
			fmt.Fprintf(os.Stderr, "cloudsourced: the self-signed certificate does not cover %s; creating a new one (clients need the new fingerprint)\n", hostname)
		}
	}
	id, err := newSelfSigned(hostname, 10*365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(id)
	return id, os.WriteFile(p, b, 0o600)
}

func certCovers(certPEM, hostname string) bool {
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		return false
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	return err == nil && c.VerifyHostname(hostname) == nil
}

// newSelfSigned creates a control plane certificate.
func newSelfSigned(hostname string, lifetime time.Duration) (*transport.Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cpCertCN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cpCertCN},
	}
	if ip := net.ParseIP(hostname); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else if hostname != "" {
		tmpl.Subject.CommonName = hostname
		tmpl.DNSNames = append(tmpl.DNSNames, hostname)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &transport.Identity{
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})),
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// --- server binary of local-docker machines ----------------------------

// setMachineBinary points local-docker providers without an explicit
// binary option at bin (a path on the Docker host).
func setMachineBinary(u *config.User, bin string) {
	if bin == "" {
		return
	}
	for name, opts := range u.Providers {
		if u.ProviderBackend(name) != "local-docker" {
			continue
		}
		if s, _ := opts["binary"].(string); s == "" {
			opts["binary"] = bin
		}
	}
}

func hasLocalDocker(u *config.User) bool {
	for _, name := range u.ProviderNames() {
		if u.ProviderBackend(name) == "local-docker" {
			return true
		}
	}
	return false
}

// inContainer reports whether the control plane runs in a container.
func inContainer() bool {
	if os.Getenv("CLOUDSOURCED_IN_CONTAINER") == "1" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// resolveMachineBinary returns the server binary local-docker machines
// mount, as a path on the Docker host ("" = provider default: this
// executable). local-docker bind mounts the binary, which the Docker daemon
// resolves on the host; inside a container this executable's path means
// nothing there. So the binary is copied into the state directory and its
// host path is derived from this container's mounts.
func resolveMachineBinary(flagValue, dir string, warn func(string, ...any)) (string, error) {
	switch {
	case flagValue == "self":
		return "", nil
	case flagValue != "":
		return flagValue, nil
	case !inContainer():
		return "", nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "bin", "cloudsourced")
	if err := copyExecutable(self, dst); err != nil {
		return "", fmt.Errorf("export machine binary: %w", err)
	}
	host, err := hostPath(dst, dockerCommand())
	if err != nil {
		warn("cannot map the machine binary to a host path; assuming the state directory is mounted at the same path on the host", "path", dst, "err", err)
		return dst, nil
	}
	return host, nil
}

func dockerCommand() []string {
	if c := strings.Fields(os.Getenv("CLOUDSOURCED_DOCKER")); len(c) > 0 {
		return c
	}
	return []string{"docker"}
}

func copyExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".cloudsourced-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

var containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/(?:hostname|hosts|resolv\.conf) `)

// ownContainerID finds this container's id in /proc/self/mountinfo: Docker
// bind mounts /etc/hostname from <data-root>/containers/<id>/, also with
// --network host, where the hostname is not the id.
func ownContainerID(mountinfo io.Reader) string {
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if m := containerIDRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

type dockerMount struct {
	Source      string
	Destination string
}

// translateMount maps a path inside the container to the host path of the
// innermost mount containing it (bind mounts and named volumes alike).
func translateMount(path string, mounts []dockerMount) (string, bool) {
	best, bestLen := -1, -1
	for i, m := range mounts {
		d := strings.TrimRight(m.Destination, "/")
		if (path == d || strings.HasPrefix(path, d+"/")) && len(d) > bestLen {
			best, bestLen = i, len(d)
		}
	}
	if best < 0 || mounts[best].Source == "" {
		return "", false
	}
	return filepath.Join(mounts[best].Source, strings.TrimPrefix(path, strings.TrimRight(mounts[best].Destination, "/"))), true
}

func hostPath(path string, docker []string) (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	id := ownContainerID(f)
	f.Close()
	if id == "" {
		return "", errors.New("container id not found in /proc/self/mountinfo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append(append([]string{}, docker[1:]...), "inspect", "--type", "container", "--format", "{{json .Mounts}}", id)
	out, err := exec.CommandContext(ctx, docker[0], argv...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("docker inspect: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	var mounts []dockerMount
	if err := json.Unmarshal(out, &mounts); err != nil {
		return "", err
	}
	p, ok := translateMount(path, mounts)
	if !ok {
		return "", fmt.Errorf("%s is not on a volume or bind mount", path)
	}
	return p, nil
}

// --- info, client-config, health ---------------------------------------

func readCPState(dir string) (*cpState, error) {
	b, err := os.ReadFile(filepath.Join(dir, cpStateFile))
	if err != nil {
		return nil, err
	}
	var st cpState
	return &st, json.Unmarshal(b, &st)
}

func controlplaneInfo(args []string) error {
	fs := flag.NewFlagSet("controlplane info", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory of the control plane")
	showKey := fs.Bool("show-key", false, "also print the API key itself")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := cpStateDir(*stateDir)
	st, err := readCPState(dir)
	if err != nil {
		return fmt.Errorf("no control plane state in %s (start \"cloudsourced controlplane\" first): %w", dir, err)
	}
	fmt.Printf("state:        %s\nversion:      %s\nstarted:      %s\nlisten:       %s\n", dir, st.Version, st.Started.Format(time.RFC3339), st.Listen)
	if st.Hostname != "" {
		fmt.Printf("hostname:     %s\n", st.Hostname)
	}
	fmt.Printf("tls:          %s\n", st.TLS)
	if st.Fingerprint != "" {
		fmt.Printf("fingerprint:  %s\n", st.Fingerprint)
	}
	for i, u := range st.URLs {
		label := "url:"
		if i > 0 {
			label = ""
		}
		fmt.Printf("%-13s %s\n", label, u)
	}
	fmt.Printf("api key:      %s\n", st.APIKey)
	if *showKey {
		path := st.APIKey
		if strings.HasPrefix(path, "$") {
			path = ""
		}
		key, _, _, err := loadAPIKey(path, dir)
		if err != nil {
			return err
		}
		fmt.Printf("api key value: %s\n", key)
	}
	fmt.Printf("clients:      cloudsourced controlplane client-config --state-dir %s\n", dir)
	return nil
}

func controlplaneClientConfig(args []string) error {
	fs := flag.NewFlagSet("controlplane client-config", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory of a control plane on this host (default URL and fingerprint)")
	url := fs.String("url", "", "control plane URL for clients (default: the first URL of the state)")
	fp := fs.String("fingerprint", "", "certificate fingerprint to pin (default: from the state, else fetched from --url unless it is publicly trusted)")
	keyRef := fs.String("api-key", "env:CLOUDSOURCED_API_KEY", "client reference to the API key (env:VAR, file:PATH, cmd:COMMAND)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	block, err := clientConfigBlock(cpStateDir(*stateDir), *url, *fp, *keyRef, probeCertificate)
	if err != nil {
		return err
	}
	fmt.Print(block)
	return nil
}

// clientConfigBlock renders the client [orchestrator] section.
func clientConfigBlock(dir, url, fp, keyRef string, probe func(string) (string, bool, error)) (string, error) {
	st, _ := readCPState(dir)
	if url == "" && st != nil && len(st.URLs) > 0 {
		url = st.URLs[0]
	}
	if url == "" {
		return "", errors.New("client-config: --url is required (no control plane state in " + dir + ")")
	}
	note := ""
	if fp == "" && strings.HasPrefix(url, "https://") {
		switch {
		case st != nil && st.TLS == "self-signed":
			fp = st.Fingerprint
		case st == nil:
			got, trusted, err := probe(url)
			if err != nil {
				return "", fmt.Errorf("client-config: %w (pass --fingerprint)", err)
			}
			if !trusted {
				fp = got
				note = "# fetched from the server: compare with \"cloudsourced controlplane info\" there\n"
			}
		}
	}
	var b strings.Builder
	b.WriteString("[orchestrator]\nmode = \"remote\"\n")
	fmt.Fprintf(&b, "url = %q\n", url)
	if fp != "" {
		b.WriteString(note)
		fmt.Fprintf(&b, "fingerprint = %q\n", fp)
	}
	fmt.Fprintf(&b, "api_key = %q\n", keyRef)
	return b.String(), nil
}

// probeCertificate returns the fingerprint of the certificate served at
// rawURL and whether the system CAs trust it for the URL's host.
func probeCertificate(rawURL string) (string, bool, error) {
	host := strings.TrimPrefix(rawURL, "https://")
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	addr := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = net.JoinHostPort(strings.Trim(host, "[]"), "443")
	}
	name, _, _ := net.SplitHostPort(addr)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: name})
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", false, errors.New("no certificate")
	}
	sum := sha256.Sum256(certs[0].Raw)
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, verr := certs[0].Verify(x509.VerifyOptions{DNSName: name, Intermediates: inter})
	return hex.EncodeToString(sum[:]), verr == nil, nil
}

// controlplaneHealth checks /healthz of the control plane of a state
// directory (container health check).
func controlplaneHealth(args []string) error {
	fs := flag.NewFlagSet("controlplane health", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "state directory of the control plane")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := readCPState(cpStateDir(*stateDir))
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(st.Listen)
	if err != nil {
		return err
	}
	target := net.JoinHostPort("127.0.0.1", port)
	if st.TLS == "acme" {
		// A TLS handshake would ask autocert for a certificate (and, before
		// the first one exists, an ACME order): only check the listener.
		conn, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			return err
		}
		conn.Close()
		fmt.Println("ok")
		return nil
	}
	scheme := "https"
	if st.TLS == "off" {
		scheme = "http"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // liveness only
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := c.Get(scheme + "://" + target + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	fmt.Println("ok")
	return nil
}
