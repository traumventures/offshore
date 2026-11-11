package blobcache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GoCacheProg implements the GOCACHEPROG protocol of the go command
// (cmd/go/internal/cacheprog): a local directory on the machine's disk
// holds the outputs go reads (DiskPath), the blobcache endpoint shares
// them across machines. Remote failures are misses, never errors.
type GoCacheProg struct {
	Dir      string       // local cache directory
	Endpoint string       // blobcache base URL ending in "/" ("" = local only)
	Client   *http.Client // nil: a client with a 30s timeout

	wg sync.WaitGroup // uploads in flight
}

type goProgRequest struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type goProgResponse struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// goActionEntry is the stored value of an action.
type goActionEntry struct {
	OutputID string    `json:"output"`
	Size     int64     `json:"size"`
	Time     time.Time `json:"time"`
}

// Serve speaks the protocol on r/w until "close" or EOF.
func (g *GoCacheProg) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if g.Client == nil {
		g.Client = &http.Client{Timeout: 30 * time.Second}
	}
	for _, d := range []string{"a", "o", "tmp"} {
		if err := os.MkdirAll(filepath.Join(g.Dir, d), 0o755); err != nil {
			return err
		}
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	send := func(resp goProgResponse) error {
		if err := enc.Encode(resp); err != nil {
			return err
		}
		return bw.Flush()
	}
	if err := send(goProgResponse{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return err
	}
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var req goProgRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				g.wg.Wait()
				return nil
			}
			return err
		}
		var resp goProgResponse
		switch req.Command {
		case "get":
			resp = g.get(ctx, req.ActionID)
		case "put":
			var body []byte
			if req.BodySize > 0 {
				if err := dec.Decode(&body); err != nil {
					return fmt.Errorf("gocacheprog: put body: %w", err)
				}
			}
			resp = g.put(ctx, req, body)
		case "close":
			g.wg.Wait()
			resp.ID = req.ID
			return send(resp)
		default:
			resp.Err = "unknown command " + req.Command
		}
		resp.ID = req.ID
		if err := send(resp); err != nil {
			return err
		}
	}
}

func (g *GoCacheProg) actionPath(id string) string { return filepath.Join(g.Dir, "a", id) }
func (g *GoCacheProg) outputPath(id string) string { return filepath.Join(g.Dir, "o", id) }

func (g *GoCacheProg) get(ctx context.Context, actionID []byte) goProgResponse {
	aid := hex.EncodeToString(actionID)
	var e goActionEntry
	b, err := os.ReadFile(g.actionPath(aid))
	if err != nil || json.Unmarshal(b, &e) != nil {
		b, err = g.fetch(ctx, "go/a/"+aid)
		if err != nil || json.Unmarshal(b, &e) != nil || !validHex(e.OutputID) {
			return goProgResponse{Miss: true}
		}
		if _, err := os.Stat(g.outputPath(e.OutputID)); err != nil {
			body, err := g.fetch(ctx, "go/o/"+e.OutputID)
			if err != nil || int64(len(body)) != e.Size || g.writeLocal(g.outputPath(e.OutputID), body) != nil {
				return goProgResponse{Miss: true}
			}
		}
		g.writeLocal(g.actionPath(aid), b)
	}
	fi, err := os.Stat(g.outputPath(e.OutputID))
	if err != nil || fi.Size() != e.Size {
		return goProgResponse{Miss: true}
	}
	oid, _ := hex.DecodeString(e.OutputID)
	t := e.Time
	return goProgResponse{OutputID: oid, Size: e.Size, Time: &t, DiskPath: g.outputPath(e.OutputID)}
}

func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && s != "" && len(s) <= 128
}

func (g *GoCacheProg) put(ctx context.Context, req goProgRequest, body []byte) goProgResponse {
	if int64(len(body)) != req.BodySize {
		return goProgResponse{Err: fmt.Sprintf("body size %d, want %d", len(body), req.BodySize)}
	}
	aid, oid := hex.EncodeToString(req.ActionID), hex.EncodeToString(req.OutputID)
	if err := g.writeLocal(g.outputPath(oid), body); err != nil {
		return goProgResponse{Err: err.Error()}
	}
	e, _ := json.Marshal(goActionEntry{OutputID: oid, Size: req.BodySize, Time: time.Now().UTC()})
	if err := g.writeLocal(g.actionPath(aid), e); err != nil {
		return goProgResponse{Err: err.Error()}
	}
	if g.Endpoint != "" {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			// Output before action: a reader that finds the action finds
			// its output.
			if g.upload(ctx, "go/o/"+oid, body) == nil {
				g.upload(ctx, "go/a/"+aid, e)
			}
		}()
	}
	return goProgResponse{DiskPath: g.outputPath(oid)}
}

func (g *GoCacheProg) writeLocal(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Join(g.Dir, "tmp"), "w-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func (g *GoCacheProg) fetch(ctx context.Context, key string) ([]byte, error) {
	if g.Endpoint == "" {
		return nil, ErrNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Endpoint+key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", key, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, DefaultMaxObject))
}

func (g *GoCacheProg) upload(ctx context.Context, key string, b []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, g.Endpoint+key, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT %s: %s", key, resp.Status)
	}
	return nil
}

// EncodeGoProgBody encodes a put body like the go command does (tests).
func EncodeGoProgBody(b []byte) string { return `"` + base64.StdEncoding.EncodeToString(b) + `"` }
