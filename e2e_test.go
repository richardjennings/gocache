package main_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richardjennings/gocache/internal/api"
	"github.com/richardjennings/gocache/internal/server"
)

// env is a gocache binary, a server, and a small module to build and test.
type env struct {
	t   *testing.T
	tmp string
	bin string
	url string
	mod string
}

func setup(t *testing.T) *env {
	t.Helper()
	if testing.Short() {
		t.Skip("runs the go command")
	}
	tmp := t.TempDir()
	e := &env{t: t, tmp: tmp, bin: filepath.Join(tmp, "gocache"), mod: filepath.Join(tmp, "mod")}
	if out, err := exec.Command("go", "build", "-o", e.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building gocache: %v\n%s", err, out)
	}
	s, err := server.New(filepath.Join(tmp, "data"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	e.url = ts.URL
	if err := os.MkdirAll(e.mod, 0o755); err != nil {
		t.Fatal(err)
	}
	// A library package: the go command never stores a linked binary in a
	// GOCACHEPROG cache, so a main package would miss its link step in every
	// build.
	for name, content := range map[string]string{
		"go.mod":      "module example.com/e2e\n\ngo 1.24\n",
		"e2e.go":      "package e2e\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\nfunc Up(s string) string { return fmt.Sprint(strings.ToUpper(s)) }\n",
		"e2e_test.go": "package e2e\n\nimport \"testing\"\n\nfunc TestUp(t *testing.T) {\n\tif got := Up(\"ok\"); got != \"OK\" {\n\t\tt.Fatal(got)\n\t}\n}\n",
	} {
		p := filepath.Join(e.mod, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		// Go does not cache its index of a module whose files changed very
		// recently. Old files make every build use the same cached index.
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// run runs a go command in the module through "gocache client", with an
// empty GOCACHE of its own. It returns the output, and an error if the go
// command fails or the client reports an error. It does not fail the test,
// so goroutines can call it.
func (e *env) run(name string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = e.mod
	cmd.Env = append(os.Environ(),
		"GOCACHEPROG="+e.bin+" client",
		"GOCACHE_SERVER="+e.url,
		"GOCACHE="+filepath.Join(e.tmp, "gocache-"+name),
		"GOFLAGS=",
		"GOTOOLCHAIN=local",
	)
	out, err := cmd.CombinedOutput()
	if err == nil && strings.Contains(string(out), "gocache client:") {
		err = fmt.Errorf("the client reported an error")
	}
	if err != nil {
		return "", fmt.Errorf("go %s (%s): %v\n%s", strings.Join(args, " "), name, err, out)
	}
	return string(out), nil
}

func (e *env) stats() api.Stats {
	e.t.Helper()
	resp, err := http.Get(e.url + "/stats")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var st api.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

// TestGoBuildUsesTheSharedCache builds the package twice. The second build
// must get every output from the server.
func TestGoBuildUsesTheSharedCache(t *testing.T) {
	e := setup(t)
	if _, err := e.run("first", "build", "."); err != nil {
		t.Fatal(err)
	}
	first := e.stats()
	if first.OutputPuts == 0 {
		t.Fatalf("the first build stored nothing: %+v", first)
	}
	if _, err := e.run("second", "build", "."); err != nil {
		t.Fatal(err)
	}
	second := e.stats()
	if second.ActionHits == first.ActionHits || second.ActionMisses != first.ActionMisses {
		t.Fatalf("the second build did not get everything from the server: before %+v, after %+v", first, second)
	}
	t.Logf("after the first build: %+v; after the second: %+v", first, second)
}

// TestGoTestUsesTheSharedCache runs the package's tests twice. The second run
// must use the cached test result and get everything from the server.
func TestGoTestUsesTheSharedCache(t *testing.T) {
	e := setup(t)
	if _, err := e.run("first", "test", "."); err != nil {
		t.Fatal(err)
	}
	first := e.stats()
	out, err := e.run("second", "test", ".")
	if err != nil {
		t.Fatal(err)
	}
	second := e.stats()
	if !strings.Contains(out, "(cached)") {
		t.Errorf("the second run did not use the cached test result:\n%s", out)
	}
	if second.ActionHits == first.ActionHits || second.ActionMisses != first.ActionMisses {
		t.Errorf("the second run did not get everything from the server: before %+v, after %+v", first, second)
	}
	t.Logf("after the first run: %+v; after the second: %+v", first, second)
}

// TestConcurrentBuildsShareTheCache builds the package four times at once, so
// the builds upload the same outputs at the same time. A fifth build must
// then get every output from the server.
func TestConcurrentBuildsShareTheCache(t *testing.T) {
	e := setup(t)
	errs := make(chan error, 4)
	for i := range 4 {
		go func() {
			_, err := e.run(fmt.Sprint(i), "build", ".")
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	before := e.stats()
	if _, err := e.run("fifth", "build", "."); err != nil {
		t.Fatal(err)
	}
	after := e.stats()
	if after.ActionHits == before.ActionHits || after.ActionMisses != before.ActionMisses {
		t.Fatalf("the fifth build did not get everything from the server: before %+v, after %+v", before, after)
	}
	t.Logf("after four builds at once: %+v; after the fifth: %+v", before, after)
}
