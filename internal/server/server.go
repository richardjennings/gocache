// Package server keeps the shared Go build cache on disk and serves it over
// HTTP (see package api).
//
// Layout under the data directory:
//
//	a/xx/<actionID>  an api.Action, as JSON
//	o/xx/<outputID>  output bytes, stored once for every action that has them
//	tmp/             files being written; each is renamed into place when done
//
// A GET sets the file's modification time, so Sweep can remove the least
// recently used files first.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/richardjennings/gocache/internal/api"
)

// maxActionBytes limits the body of an action PUT. An Action is about 150
// bytes of JSON.
const maxActionBytes = 4 << 10

// Server serves one data directory.
type Server struct {
	dir      string
	maxBytes int64

	actionHits   atomic.Int64
	actionMisses atomic.Int64
	outputPuts   atomic.Int64
}

// New prepares dir and returns a Server whose Sweep keeps dir under maxBytes.
func New(dir string, maxBytes int64) (*Server, error) {
	// A crash can leave half-written files in tmp. Nothing refers to them.
	if err := os.RemoveAll(filepath.Join(dir, "tmp")); err != nil {
		return nil, err
	}
	for _, d := range []string{"a", "o", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Server{dir: dir, maxBytes: maxBytes}, nil
}

// Handler returns the HTTP handler for the api routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{id}", s.getAction)
	mux.HandleFunc("PUT /a/{id}", s.putAction)
	mux.HandleFunc("GET /o/{id}", s.getOutput)
	mux.HandleFunc("PUT /o/{id}", s.putOutput)
	mux.HandleFunc("GET /stats", s.stats)
	return mux
}

func (s *Server) path(kind, id string) string {
	return filepath.Join(s.dir, kind, id[:2], id)
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !api.ValidID(id) {
		http.Error(w, "bad action ID", http.StatusBadRequest)
		return
	}
	p := s.path("a", id)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		s.actionMisses.Add(1)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var a api.Action
	if err := json.Unmarshal(b, &a); err != nil || !api.ValidID(a.OutputID) {
		http.Error(w, "stored action is corrupt", http.StatusInternalServerError)
		return
	}
	// Sweep can remove an output and leave its actions. Such an action is a
	// miss, and it goes too.
	if _, err := os.Stat(s.path("o", a.OutputID)); err != nil {
		os.Remove(p)
		s.actionMisses.Add(1)
		http.NotFound(w, r)
		return
	}
	s.actionHits.Add(1)
	touch(p)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) putAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !api.ValidID(id) {
		http.Error(w, "bad action ID", http.StatusBadRequest)
		return
	}
	var a api.Action
	if err := json.NewDecoder(io.LimitReader(r.Body, maxActionBytes)).Decode(&a); err != nil {
		http.Error(w, "bad action: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !api.ValidID(a.OutputID) || a.Size < 0 {
		http.Error(w, "bad action", http.StatusBadRequest)
		return
	}
	// The client uploads the output before the action. An action whose output
	// is not stored could never be a hit.
	if _, err := os.Stat(s.path("o", a.OutputID)); err != nil {
		http.Error(w, "output not stored", http.StatusConflict)
		return
	}
	b, err := json.Marshal(a)
	if err == nil {
		err = s.place(s.path("a", id), bytes.NewReader(b), nil)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !api.ValidID(id) {
		http.Error(w, "bad output ID", http.StatusBadRequest)
		return
	}
	p := s.path("o", id)
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	touch(p)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	io.Copy(w, f)
}

func (s *Server) putOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !api.ValidID(id) {
		http.Error(w, "bad output ID", http.StatusBadRequest)
		return
	}
	// The go command names an output by the SHA-256 of its bytes, so the
	// server can refuse a body that a failed upload cut short.
	h := sha256.New()
	err := s.place(s.path("o", id), r.Body, func() error {
		if got := hex.EncodeToString(h.Sum(nil)); got != id {
			return errMismatch
		}
		return nil
	}, h)
	if errors.Is(err, errMismatch) {
		http.Error(w, "body does not match the output ID", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.outputPuts.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

var errMismatch = errors.New("content does not match its ID")

// place writes r to a file in tmp, runs check if it is not nil, and renames
// the file to final. Readers see either no file or the whole file. Extra
// writers also receive everything that is written.
func (s *Server) place(final string, r io.Reader, check func() error, extra ...io.Writer) error {
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // does nothing after a successful rename
	_, err = io.Copy(io.MultiWriter(append([]io.Writer{tmp}, extra...)...), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && check != nil {
		err = check()
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(final), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), final)
	}
	return err
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(api.Stats{
		ActionHits:   s.actionHits.Load(),
		ActionMisses: s.actionMisses.Load(),
		OutputPuts:   s.outputPuts.Load(),
	})
}

func touch(p string) {
	now := time.Now()
	os.Chtimes(p, now, now)
}

// Sweep does nothing while the stored files total at most maxBytes. Above
// that, it removes the least recently used files until they total at most
// 90% of maxBytes, and returns how many it removed.
func (s *Server) Sweep() (int, error) {
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	for _, kind := range []string{"a", "o"} {
		err := filepath.WalkDir(filepath.Join(s.dir, kind), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return nil // removed while the walk ran
			}
			files = append(files, file{p, info.Size(), info.ModTime()})
			total += info.Size()
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if total <= s.maxBytes {
		return 0, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	removed := 0
	for _, f := range files {
		if total <= s.maxBytes/10*9 {
			break
		}
		if err := os.Remove(f.path); err == nil || errors.Is(err, fs.ErrNotExist) {
			total -= f.size
			removed++
		}
	}
	return removed, nil
}
