package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richardjennings/gocache/internal/api"
)

func newTestServer(t *testing.T, maxBytes int64) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var actionID = strings.Repeat("ab", 32)

func putAction(t *testing.T, url, outputID string, size int64) int {
	t.Helper()
	b, _ := json.Marshal(api.Action{OutputID: outputID, Size: size, Time: time.Now().UTC()})
	code, _ := do(t, http.MethodPut, url+api.ActionPath(actionID), b)
	return code
}

func TestPutGet(t *testing.T) {
	s, ts := newTestServer(t, 1<<30)
	body := []byte("compiled package")
	out := sum(body)

	if code, _ := do(t, http.MethodPut, ts.URL+api.OutputPath(out), body); code != http.StatusNoContent {
		t.Fatalf("PUT output: %d", code)
	}
	if code := putAction(t, ts.URL, out, int64(len(body))); code != http.StatusNoContent {
		t.Fatalf("PUT action: %d", code)
	}
	code, b := do(t, http.MethodGet, ts.URL+api.ActionPath(actionID), nil)
	var a api.Action
	if code != http.StatusOK || json.Unmarshal(b, &a) != nil || a.OutputID != out || a.Size != int64(len(body)) {
		t.Fatalf("GET action: %d %s", code, b)
	}
	if code, b := do(t, http.MethodGet, ts.URL+api.OutputPath(out), nil); code != http.StatusOK || !bytes.Equal(b, body) {
		t.Fatalf("GET output: %d %q", code, b)
	}
	if s.actionHits.Load() != 1 || s.outputPuts.Load() != 1 {
		t.Errorf("hits %d, puts %d; want 1, 1", s.actionHits.Load(), s.outputPuts.Load())
	}
}

func TestOutputThatDoesNotMatchItsIDIsRefused(t *testing.T) {
	_, ts := newTestServer(t, 1<<30)
	out := sum([]byte("the real bytes"))
	if code, _ := do(t, http.MethodPut, ts.URL+api.OutputPath(out), []byte("cut sh")); code != http.StatusBadRequest {
		t.Fatalf("PUT output: %d, want 400", code)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+api.OutputPath(out), nil); code != http.StatusNotFound {
		t.Fatalf("GET output: %d, want 404", code)
	}
}

func TestActionWithoutItsOutputIsRefused(t *testing.T) {
	_, ts := newTestServer(t, 1<<30)
	if code := putAction(t, ts.URL, sum([]byte("never uploaded")), 14); code != http.StatusConflict {
		t.Fatalf("PUT action: %d, want 409", code)
	}
}

func TestMiss(t *testing.T) {
	s, ts := newTestServer(t, 1<<30)
	if code, _ := do(t, http.MethodGet, ts.URL+api.ActionPath(actionID), nil); code != http.StatusNotFound {
		t.Fatalf("GET action: %d, want 404", code)
	}
	if s.actionMisses.Load() != 1 {
		t.Errorf("misses %d, want 1", s.actionMisses.Load())
	}
}

func TestActionWhoseOutputWasSweptIsAMiss(t *testing.T) {
	s, ts := newTestServer(t, 1<<30)
	body := []byte("output")
	out := sum(body)
	do(t, http.MethodPut, ts.URL+api.OutputPath(out), body)
	putAction(t, ts.URL, out, int64(len(body)))
	if err := os.Remove(s.path("o", out)); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+api.ActionPath(actionID), nil); code != http.StatusNotFound {
		t.Fatalf("GET action: %d, want 404", code)
	}
	if _, err := os.Stat(s.path("a", actionID)); !os.IsNotExist(err) {
		t.Errorf("the action is still stored: %v", err)
	}
}

func TestBadIDs(t *testing.T) {
	_, ts := newTestServer(t, 1<<30)
	for _, p := range []string{"/a/abc", "/o/" + strings.Repeat("A", 64), "/a/" + strings.Repeat("g", 64)} {
		if code, _ := do(t, http.MethodGet, ts.URL+p, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", p, code)
		}
	}
}

func TestSweepRemovesTheLeastRecentlyUsedFiles(t *testing.T) {
	s, err := New(t.TempDir(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Five 300-byte outputs, used one minute apart: 1500 bytes against a
	// 1000-byte limit. The sweep must go down to 900, so the oldest two go.
	start := time.Now().Add(-time.Hour)
	var ids []string
	for i := range 5 {
		b := bytes.Repeat([]byte{byte(i)}, 300)
		id := sum(b)
		p := s.path("o", id)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		used := start.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, used, used); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	n, err := s.Sweep()
	if err != nil || n != 2 {
		t.Fatalf("Sweep: %d, %v; want 2, nil", n, err)
	}
	for i, id := range ids {
		_, err := os.Stat(s.path("o", id))
		if kept := err == nil; kept != (i >= 2) {
			t.Errorf("output %d: kept %v", i, kept)
		}
	}
	if n, err := s.Sweep(); err != nil || n != 0 {
		t.Errorf("second Sweep: %d, %v; want 0, nil", n, err)
	}
}

// try is do without t.Fatal, so goroutines can call it.
func try(method, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func TestConcurrentUse(t *testing.T) {
	// With a 1-byte limit, every sweep removes every file. So files go
	// while uploads and reads are in progress.
	s, ts := newTestServer(t, 1)
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB, so writes and reads overlap
	out := sum(body)
	actionJSON, _ := json.Marshal(api.Action{OutputID: out, Size: int64(len(body)), Time: time.Now().UTC()})

	errs := make(chan error, 1000)
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { // uploads the output, then the action
			defer wg.Done()
			if code, _, err := try(http.MethodPut, ts.URL+api.OutputPath(out), body); err != nil || code != http.StatusNoContent {
				errs <- fmt.Errorf("PUT output: %d %v", code, err)
			}
			// 409 is correct when a sweep removed the output in between.
			code, _, err := try(http.MethodPut, ts.URL+api.ActionPath(actionID), actionJSON)
			if err != nil || code != http.StatusNoContent && code != http.StatusConflict {
				errs <- fmt.Errorf("PUT action: %d %v", code, err)
			}
		}()
		go func() { // reads: each answer is either complete or 404
			defer wg.Done()
			code, b, err := try(http.MethodGet, ts.URL+api.OutputPath(out), nil)
			if err != nil || code == http.StatusOK && !bytes.Equal(b, body) || code != http.StatusOK && code != http.StatusNotFound {
				errs <- fmt.Errorf("GET output: %d, %d bytes, %v", code, len(b), err)
			}
			code, _, err = try(http.MethodGet, ts.URL+api.ActionPath(actionID), nil)
			if err != nil || code != http.StatusOK && code != http.StatusNotFound {
				errs <- fmt.Errorf("GET action: %d %v", code, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			if _, err := s.Sweep(); err != nil {
				errs <- fmt.Errorf("Sweep: %v", err)
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
