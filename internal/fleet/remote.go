package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/traumventures/cloudsourced/internal/orchestrator"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

// Remote is a Fleet served by a shared control plane.
type Remote struct {
	base   string
	apiKey string
	http   *http.Client
}

var _ Fleet = (*Remote)(nil)

// NewRemote connects to the control plane at baseURL. A non-empty
// fingerprint pins its (self-signed) certificate instead of using the
// system certificate authorities. Client certificates are presented to
// control planes that require mutual TLS.
func NewRemote(baseURL, fingerprint, apiKey string, clientCerts ...tls.Certificate) (*Remote, error) {
	if _, err := url.Parse(baseURL); err != nil {
		return nil, err
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: clientCerts}
	if fingerprint != "" {
		want := strings.ToLower(strings.TrimPrefix(fingerprint, "sha256:"))
		tc.InsecureSkipVerify = true // replaced by fingerprint pinning
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no control plane certificate")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("control plane certificate fingerprint mismatch")
			}
			return nil
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Remote{base: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: &http.Client{Transport: tr}}, nil
}

// call posts body to path and streams progress until the result frame.
func (r *Remote) call(ctx context.Context, path string, body io.Reader, contentType string, progress io.Writer, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control plane: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var f Frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return fmt.Errorf("control plane: %w", err)
		}
		if f.Progress != "" && progress != nil {
			io.WriteString(progress, f.Progress)
		}
		if !f.Done {
			continue
		}
		if f.Error != "" {
			return errors.New(f.Error)
		}
		if out != nil && len(f.Result) > 0 {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("control plane: %w", err)
	}
	return errors.New("control plane closed the connection")
}

func (r *Remote) callJSON(ctx context.Context, path string, in any, progress io.Writer, out any) error {
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return r.call(ctx, path, body, "application/json", progress, out)
}

// EnsureEnv asks the control plane for the image and uploads the build
// context only when it does not have it yet.
func (r *Remote) EnsureEnv(ctx context.Context, env Env, progress io.Writer) error {
	q := pathEnv + "?" + url.Values{"hash": {env.Hash}, "tag": {env.Tag}}.Encode()
	var res envResult
	if err := r.call(ctx, q, http.NoBody, "", progress, &res); err != nil {
		return err
	}
	if !res.NeedContext {
		return nil
	}
	spec, dir, err := env.Context()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(PackEnv(pw, spec)) }()
	defer pr.Close()
	fmt.Fprintf(progress, "cloudsourced: uploading environment %s to the control plane\n", env.Tag)
	res = envResult{}
	if err := r.call(ctx, q, pr, "application/gzip", progress, &res); err != nil {
		return err
	}
	if res.NeedContext {
		return errors.New("control plane did not accept the environment context")
	}
	return nil
}

func (r *Remote) Acquire(ctx context.Context, req orchestrator.Request, progress io.Writer) (orchestrator.Lease, error) {
	var lease orchestrator.Lease
	err := r.callJSON(ctx, pathAcquire, req, progress, &lease)
	return lease, err
}

func (r *Remote) Release(ctx context.Context, leaseID, usageKey string, usage *scheduler.Usage) error {
	return r.callJSON(ctx, pathRelease, releaseReq{Lease: leaseID, UsageKey: usageKey, Usage: usage}, nil, nil)
}

func (r *Remote) Unreachable(ctx context.Context, leaseID, machineID string) error {
	return r.callJSON(ctx, pathUnreachable, unreachableReq{Lease: leaseID, Machine: machineID}, nil, nil)
}

func (r *Remote) Estimate(ctx context.Context, key string, fallback scheduler.Resources) (scheduler.Resources, error) {
	var res scheduler.Resources
	err := r.callJSON(ctx, pathEstimate, estimateReq{Key: key, Fallback: fallback}, nil, &res)
	return res, err
}

func (r *Remote) Status(ctx context.Context) ([]orchestrator.Machine, error) {
	var ms []orchestrator.Machine
	err := r.callJSON(ctx, pathStatus, nil, nil, &ms)
	return ms, err
}

func (r *Remote) Reconcile(ctx context.Context) error {
	return r.callJSON(ctx, pathReconcile, nil, nil, nil)
}

func (r *Remote) DestroyAll(ctx context.Context) error {
	return r.callJSON(ctx, pathDown, nil, nil, nil)
}
