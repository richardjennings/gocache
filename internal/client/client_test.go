package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/richardjennings/gocache/internal/api"
	"github.com/richardjennings/gocache/internal/server"
)

// session plays the go command's side of the protocol against one Run.
type session struct {
	t      *testing.T
	enc    *json.Encoder
	dec    *json.Decoder
	done   chan error
	errOut *bytes.Buffer
}

func start(t *testing.T, remote string) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, enc: json.NewEncoder(inW), dec: json.NewDecoder(outR), done: make(chan error, 1), errOut: &bytes.Buffer{}}
	go func() {
		err := Run(inR, outW, s.errOut, remote)
		outW.Close()
		s.done <- err
	}()
	if r := s.read(); r.ID != 0 || !reflect.DeepEqual(r.KnownCommands, []cmd{cmdGet, cmdPut, cmdClose}) {
		t.Fatalf("first message: %+v", r)
	}
	return s
}

func (s *session) read() response {
	s.t.Helper()
	var r response
	if err := s.dec.Decode(&r); err != nil {
		s.t.Fatal(err)
	}
	return r
}

func (s *session) send(v any) {
	s.t.Helper()
	if err := s.enc.Encode(v); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) put(id int64, action []byte, body []byte) response {
	s.t.Helper()
	out := sha256.Sum256(body)
	s.send(request{ID: id, Command: cmdPut, ActionID: action, OutputID: out[:], BodySize: int64(len(body))})
	if len(body) > 0 {
		s.send(body) // a []byte encodes as a base64 JSON string, as the go command sends it
	}
	return s.read()
}

func (s *session) get(id int64, action []byte) response {
	s.t.Helper()
	s.send(request{ID: id, Command: cmdGet, ActionID: action})
	return s.read()
}

func (s *session) close(id int64) {
	s.t.Helper()
	s.send(request{ID: id, Command: cmdClose})
	if r := s.read(); r.ID != id || r.Err != "" {
		s.t.Fatalf("close: %+v", r)
	}
	if err := <-s.done; err != nil {
		s.t.Fatalf("Run: %v", err)
	}
}

func actionID(name string) []byte {
	s := sha256.Sum256([]byte(name))
	return s[:]
}

func newServer(t *testing.T) string {
	t.Helper()
	s, err := server.New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutInOneRunIsAHitInTheNext(t *testing.T) {
	url := newServer(t)
	body := []byte("compiled package bytes")
	out := sha256.Sum256(body)

	first := start(t, url)
	r := first.put(1, actionID("a"), body)
	if r.ID != 1 || r.Err != "" || !bytes.Equal(readFile(t, r.DiskPath), body) {
		t.Fatalf("put: %+v", r)
	}
	first.close(2) // waits for the upload

	second := start(t, url)
	r = second.get(1, actionID("a"))
	if r.Miss || r.Err != "" || !bytes.Equal(r.OutputID, out[:]) || r.Size != int64(len(body)) || r.Time == nil {
		t.Fatalf("get: %+v", r)
	}
	if got := readFile(t, r.DiskPath); !bytes.Equal(got, body) {
		t.Fatalf("DiskPath holds %q", got)
	}
	second.close(2)
}

func TestUnknownActionIsAMiss(t *testing.T) {
	s := start(t, newServer(t))
	if r := s.get(1, actionID("never put")); !r.Miss || r.Err != "" {
		t.Fatalf("get: %+v", r)
	}
	s.close(2)
	if s.errOut.Len() != 0 {
		t.Errorf("a plain miss was reported as an error: %s", s.errOut)
	}
}

func TestWithoutAServerPutsStayLocal(t *testing.T) {
	s := start(t, "")
	body := []byte("local only")
	if r := s.put(1, actionID("a"), body); r.Err != "" || !bytes.Equal(readFile(t, r.DiskPath), body) {
		t.Fatalf("put: %+v", r)
	}
	if r := s.get(2, actionID("a")); !r.Miss {
		t.Fatalf("get: %+v", r)
	}
	s.close(3)
}

func TestMissesUseOneConnection(t *testing.T) {
	s, err := server.New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	var conns atomic.Int64
	ts.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)

	// One get at a time, so the transport can use the same connection for
	// each, but only if the client reads each 404 to the end.
	c := start(t, ts.URL)
	for i := range 20 {
		if r := c.get(int64(i+1), actionID(fmt.Sprint(i))); !r.Miss {
			t.Fatalf("get: %+v", r)
		}
	}
	c.close(21)
	if n := conns.Load(); n != 1 {
		t.Errorf("20 misses used %d connections, want 1", n)
	}
}

func TestServerThatIsDownGivesMisses(t *testing.T) {
	// The listener closes each connection at once, so each request fails in
	// the transport.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			conn.Close()
		}
	}()
	url := "http://" + ln.Addr().String()

	s := start(t, url)
	for i := range 10 {
		if r := s.get(int64(i+1), actionID(fmt.Sprint(i))); !r.Miss || r.Err != "" {
			t.Fatalf("get: %+v", r)
		}
	}
	body := []byte("still builds")
	if r := s.put(20, actionID("p"), body); r.Err != "" || !bytes.Equal(readFile(t, r.DiskPath), body) {
		t.Fatalf("put: %+v", r)
	}
	s.close(21)
	if n := conns.Load(); n != 1 {
		t.Errorf("the client connected %d times; after the first failure it must stop asking", n)
	}
	if !strings.Contains(s.errOut.String(), url) {
		t.Errorf("no error report: %q", s.errOut)
	}
}

func TestDownloadThatDoesNotMatchItsIDIsAMiss(t *testing.T) {
	real := []byte("the real output")
	sum := sha256.Sum256(real)
	out := api.ID(sum[:])
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Action{OutputID: out, Size: int64(len(real)), Time: time.Now()})
	})
	mux.HandleFunc("GET /o/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("different bytes")) // same length, other content
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	s := start(t, ts.URL)
	if r := s.get(1, actionID("a")); !r.Miss {
		t.Fatalf("get: %+v", r)
	}
	s.close(2)
}

func TestUnknownCommandIsAnError(t *testing.T) {
	s := start(t, "")
	s.send(request{ID: 1, Command: "get2"})
	if r := s.read(); r.ID != 1 || r.Err == "" {
		t.Fatalf("response: %+v", r)
	}
	s.close(2)
}

func TestOutputFilesGoAtClose(t *testing.T) {
	s := start(t, "")
	r := s.put(1, actionID("a"), []byte("temporary"))
	s.close(2)
	if _, err := os.Stat(r.DiskPath); !os.IsNotExist(err) {
		t.Errorf("%s is still there after close: %v", r.DiskPath, err)
	}
}

func TestManyRequestsInFlight(t *testing.T) {
	url := newServer(t)
	const stored, n = 50, 100
	bodies := make([][]byte, stored)
	seed := start(t, url)
	for i := range bodies {
		bodies[i] = []byte(fmt.Sprintf("output %d", i))
		if r := seed.put(int64(i+1), actionID(fmt.Sprint(i)), bodies[i]); r.Err != "" {
			t.Fatalf("put: %+v", r)
		}
	}
	seed.close(stored + 1)

	// The go command sends many gets without waiting for answers, and it
	// reads the answers while it sends. So this test sends from another
	// goroutine. The first 50 actions are stored; the other 50 are not.
	s := start(t, url)
	sent := make(chan error, 1)
	go func() {
		for i := range n {
			if err := s.enc.Encode(request{ID: int64(i + 1), Command: cmdGet, ActionID: actionID(fmt.Sprint(i))}); err != nil {
				sent <- err
				return
			}
		}
		sent <- nil
	}()
	got := make(map[int64]response, n)
	for range n {
		r := s.read()
		got[r.ID] = r
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	for i := range n {
		r, ok := got[int64(i+1)]
		switch {
		case !ok:
			t.Errorf("no answer to request %d", i+1)
		case i < stored && (r.Miss || !bytes.Equal(readFile(t, r.DiskPath), bodies[i])):
			t.Errorf("request %d: %+v", i+1, r)
		case i >= stored && !r.Miss:
			t.Errorf("request %d: want a miss, got %+v", i+1, r)
		}
	}
	s.close(n + 1)
}
