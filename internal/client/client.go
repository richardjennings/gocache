// Package client implements "gocache client", the program that the go
// command starts through GOCACHEPROG. It answers the go command's cache
// requests from a gocache server, and it uploads new outputs to that server.
//
// A problem with the server never fails a build. A get that cannot reach the
// server is a miss, and the go command then builds the output itself. After
// the first connection error or timeout, the client stops asking the server.
// So a server that is down or stuck delays a go command once, not once per
// request.
package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richardjennings/gocache/internal/api"
)

const (
	// The go command sends many requests at once. The server runs on the
	// same host, so a few dozen connections keep the client busy.
	maxRequests = 32
	maxUploads  = 8

	dialTimeout = 2 * time.Second
	// headerTimeout limits the wait for a response after the client sends a
	// request. The server does little work before it answers, so a longer
	// wait means that it is stuck.
	headerTimeout  = 10 * time.Second
	requestTimeout = 5 * time.Minute
	// uploadTimeout is how long the client waits for its uploads before it
	// answers close.
	uploadTimeout = time.Minute

	// maxDiscard limits how much of an unused response body the client reads
	// to keep the connection.
	maxDiscard = 64 << 10
)

type client struct {
	remote string // base URL of the server; "" means no server
	hc     *http.Client
	dir    string // holds the output files that DiskPath names

	enc   *json.Encoder
	encMu sync.Mutex

	requests sync.WaitGroup
	uploads  sync.WaitGroup
	reqSlots chan struct{}
	upSlots  chan struct{}

	down     atomic.Bool // a connection to the server failed
	errOnce  sync.Once
	firstErr error
}

// Run answers the requests that the go command writes to in, and writes the
// responses to out, until the go command sends close or closes in. remote is
// the base URL of the server, or "" for no server. Run reports the first
// server error to errOut before it returns.
func Run(in io.Reader, out, errOut io.Writer, remote string) error {
	dir, err := os.MkdirTemp("", "gocache-client-")
	if err != nil {
		return err
	}
	// The go command uses these files only while it runs. Removing them
	// keeps them out of the layer of a Docker build step.
	defer os.RemoveAll(dir)

	c := &client{
		remote:   strings.TrimRight(remote, "/"),
		hc:       newHTTPClient(),
		dir:      dir,
		enc:      json.NewEncoder(out),
		reqSlots: make(chan struct{}, maxRequests),
		upSlots:  make(chan struct{}, maxUploads),
	}
	if err := c.send(response{KnownCommands: []cmd{cmdGet, cmdPut, cmdClose}}); err != nil {
		return err
	}
	closeReq, err := c.serve(in)
	// All requests and uploads end before the client reports errors, answers
	// close, and removes the output files.
	c.finish()
	if c.firstErr != nil {
		fmt.Fprintf(errOut, "gocache client: server %s: %v\n", c.remote, c.firstErr)
	}
	if err == nil && closeReq != nil {
		err = c.send(response{ID: closeReq.ID})
	}
	return err
}

// serve reads requests until the go command sends close or closes in. It
// returns the close request, or nil at the end of in.
func (c *client) serve(in io.Reader) (*request, error) {
	dec := json.NewDecoder(in)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return nil, fmt.Errorf("reading a request: %w", err)
		}
		switch req.Command {
		case cmdGet:
			c.async(func() { c.get(req) })
		case cmdPut:
			var body []byte
			if req.BodySize > 0 {
				if err := dec.Decode(&body); err != nil {
					return nil, fmt.Errorf("reading the body of request %d: %w", req.ID, err)
				}
			}
			if int64(len(body)) != req.BodySize {
				return nil, fmt.Errorf("request %d has a %d-byte body, not %d", req.ID, len(body), req.BodySize)
			}
			c.async(func() { c.put(req, body) })
		case cmdClose:
			return &req, nil
		default:
			if err := c.send(response{ID: req.ID, Err: "unknown command " + string(req.Command)}); err != nil {
				return nil, err
			}
		}
	}
}

func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // the server is on the local network
	t.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	t.ResponseHeaderTimeout = headerTimeout
	t.MaxIdleConnsPerHost = maxRequests + maxUploads
	return &http.Client{Transport: t, Timeout: requestTimeout}
}

func (c *client) send(r response) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.enc.Encode(r)
}

// async runs f in a goroutine, at most maxRequests at a time.
func (c *client) async(f func()) {
	c.requests.Add(1)
	c.reqSlots <- struct{}{}
	go func() {
		defer func() {
			<-c.reqSlots
			c.requests.Done()
		}()
		f()
	}()
}

// finish waits for the requests in progress, then for the uploads, but not
// longer than uploadTimeout.
func (c *client) finish() {
	c.requests.Wait()
	done := make(chan struct{})
	go func() {
		c.uploads.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(uploadTimeout):
		c.fail(fmt.Errorf("uploads still running after %v", uploadTimeout), false)
	}
}

// fail records the first server error. A connection error also stops the
// client from asking the server again.
func (c *client) fail(err error, connection bool) {
	c.errOnce.Do(func() { c.firstErr = err })
	if connection {
		c.down.Store(true)
	}
}

func (c *client) useServer() bool { return c.remote != "" && !c.down.Load() }

func (c *client) get(req request) {
	miss := response{ID: req.ID, Miss: true}
	if !c.useServer() {
		c.send(miss)
		return
	}
	a, err := c.fetchAction(api.ID(req.ActionID))
	if err == nil {
		var p string
		if p, err = c.fetchOutput(a.OutputID, a.Size); err == nil {
			raw, _ := hex.DecodeString(a.OutputID)
			c.send(response{ID: req.ID, OutputID: raw, Size: a.Size, Time: &a.Time, DiskPath: p})
			return
		}
	}
	if !errors.Is(err, errNotStored) {
		c.fail(err, isConnection(err))
	}
	c.send(miss)
}

func (c *client) put(req request, body []byte) {
	outputID := api.ID(req.OutputID)
	if !api.ValidID(outputID) {
		c.send(response{ID: req.ID, Err: "put has no valid output ID"})
		return
	}
	p, err := c.store(outputID, bytes.NewReader(body), nil)
	if err != nil {
		c.send(response{ID: req.ID, Err: err.Error()})
		return
	}
	c.send(response{ID: req.ID, DiskPath: p})
	if !c.useServer() {
		return
	}
	actionID := api.ID(req.ActionID)
	size := int64(len(body))
	c.uploads.Add(1)
	go func() {
		defer c.uploads.Done()
		c.upSlots <- struct{}{}
		defer func() { <-c.upSlots }()
		if err := c.upload(actionID, outputID, p, size); err != nil {
			c.fail(err, isConnection(err))
		}
	}()
}

// errNotStored means the server holds no such action or output: a plain miss.
var errNotStored = errors.New("not stored")

func (c *client) fetchAction(id string) (api.Action, error) {
	var a api.Action
	resp, err := c.hc.Get(c.remote + api.ActionPath(id))
	if err != nil {
		return a, err
	}
	defer discard(resp)
	if resp.StatusCode == http.StatusNotFound {
		return a, errNotStored
	}
	if resp.StatusCode != http.StatusOK {
		return a, fmt.Errorf("GET action: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return a, fmt.Errorf("GET action: %w", err)
	}
	if !api.ValidID(a.OutputID) || a.Size < 0 {
		return a, errors.New("GET action: the server sent a bad action")
	}
	return a, nil
}

// fetchOutput returns the local path of an output, and downloads it first
// if the client does not have it yet.
func (c *client) fetchOutput(id string, size int64) (string, error) {
	p := filepath.Join(c.dir, id)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	resp, err := c.hc.Get(c.remote + api.OutputPath(id))
	if err != nil {
		return "", err
	}
	defer discard(resp)
	if resp.StatusCode == http.StatusNotFound {
		return "", errNotStored
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET output: %s", resp.Status)
	}
	// The go command trusts the file at DiskPath, so the client checks the
	// bytes against the output ID before it names the file.
	sum := sha256.New()
	return c.store(id, io.TeeReader(resp.Body, sum), func(written int64) error {
		if written != size || hex.EncodeToString(sum.Sum(nil)) != id {
			return fmt.Errorf("GET output: %d bytes that do not match the output ID", written)
		}
		return nil
	})
}

// store writes r to the output file for id, if it is not there yet. check,
// if not nil, sees the byte count before the file gets its final name.
func (c *client) store(id string, r io.Reader, check func(int64) error) (string, error) {
	p := filepath.Join(c.dir, id)
	if _, err := os.Stat(p); err == nil {
		io.Copy(io.Discard, r)
		return p, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(c.dir, "tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // does nothing after a successful rename
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && check != nil {
		err = check(n)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		return "", err
	}
	return p, nil
}

// upload sends the output, then the action that names it.
func (c *client) upload(actionID, outputID, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequest(http.MethodPut, c.remote+api.OutputPath(outputID), f)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if err := c.do(req); err != nil {
		return fmt.Errorf("PUT output: %w", err)
	}
	b, err := json.Marshal(api.Action{OutputID: outputID, Size: size, Time: time.Now().UTC()})
	if err != nil {
		return err
	}
	req, err = http.NewRequest(http.MethodPut, c.remote+api.ActionPath(actionID), bytes.NewReader(b))
	if err != nil {
		return err
	}
	if err := c.do(req); err != nil {
		return fmt.Errorf("PUT action: %w", err)
	}
	return nil
}

func (c *client) do(req *http.Request) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer discard(resp)
	if resp.StatusCode/100 != 2 {
		return errors.New(resp.Status)
	}
	return nil
}

// discard reads the rest of a response body, up to maxDiscard bytes, and
// closes it. The transport keeps a connection only if the client reads the
// body to the end.
func discard(resp *http.Response) {
	io.CopyN(io.Discard, resp.Body, maxDiscard)
	resp.Body.Close()
}

// isConnection reports whether err came from the transport, not from an HTTP
// status or a bad body. http.Client returns transport errors as *url.Error,
// which is a net.Error.
func isConnection(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}
