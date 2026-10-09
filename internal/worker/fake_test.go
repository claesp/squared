package worker

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"squared/internal/forgejo"
	"squared/internal/kube"
)

// fakeForgejo is an in-memory Git server with the parts of the Forgejo API
// that the worker uses. Each commit has the complete file set.
type fakeForgejo struct {
	mu      sync.Mutex
	repos   map[string][]fakeCommit // "org/repo" to commits, the newest last
	commits int
}

type fakeCommit struct {
	sha   string
	files map[string][]byte
	msg   string
}

func newFakeForgejo() *fakeForgejo { return &fakeForgejo{repos: map[string][]fakeCommit{}} }

func (f *fakeForgejo) head(repo string) fakeCommit {
	c := f.repos[repo]
	return c[len(c)-1]
}

// commitCount returns the number of commits of a repository.
func (f *fakeForgejo) commitCount(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.repos[repo])
}

// file returns a file at the head of a repository.
func (f *fakeForgejo) file(repo, path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head(repo).files[path]
}

func (f *fakeForgejo) newSHA() string {
	f.commits++
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprint(f.commits))))
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "orgs":
		w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "repos":
		if _, ok := f.repos[parts[1]+"/"+parts[2]]; !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPatch && len(parts) == 3 && parts[0] == "repos":
		w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "repos":
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		f.repos[parts[1]+"/"+in.Name] = []fakeCommit{{sha: f.newSHA(), files: map[string][]byte{"README.md": []byte("x")}}}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && len(parts) == 5 && parts[3] == "branches":
		writeJSON(w, map[string]any{"commit": map[string]string{"id": f.head(parts[1] + "/" + parts[2]).sha}})
	case r.Method == http.MethodGet && len(parts) == 6 && parts[3] == "git" && parts[4] == "trees":
		repo := parts[1] + "/" + parts[2]
		for _, c := range f.repos[repo] {
			if c.sha != parts[5] {
				continue
			}
			var tree []map[string]string
			for path, content := range c.files {
				tree = append(tree, map[string]string{"path": path, "type": "blob", "sha": forgejo.BlobSHA(content)})
			}
			writeJSON(w, map[string]any{"tree": tree, "truncated": false})
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "contents":
		repo := parts[1] + "/" + parts[2]
		var in struct {
			Message string
			Files   []struct{ Operation, Path, Content, SHA string }
		}
		json.NewDecoder(r.Body).Decode(&in)
		files := map[string][]byte{}
		for k, v := range f.head(repo).files {
			files[k] = v
		}
		for _, ch := range in.Files {
			old, exists := files[ch.Path]
			switch ch.Operation {
			case "create":
				if exists {
					http.Error(w, "exists", http.StatusUnprocessableEntity)
					return
				}
			case "update", "delete":
				if !exists || forgejo.BlobSHA(old) != ch.SHA {
					http.Error(w, "sha mismatch", http.StatusConflict)
					return
				}
			}
			if ch.Operation == "delete" {
				delete(files, ch.Path)
				continue
			}
			b, _ := base64.StdEncoding.DecodeString(ch.Content)
			files[ch.Path] = b
		}
		c := fakeCommit{sha: f.newSHA(), files: files, msg: in.Message}
		f.repos[repo] = append(f.repos[repo], c)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"commit": map[string]string{"sha": c.sha}})
	default:
		http.Error(w, "fake forgejo: no route "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

// fakeHarbor knows projects, robots and image tags.
type fakeHarbor struct {
	mu       sync.Mutex
	projects map[string]int64
	tags     map[string]string // "project/repo:tag" to digest
	robots   int
}

func (h *fakeHarbor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v2.0")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "projects":
		id, ok := h.projects[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"project_id": id, "name": parts[1]})
	case r.Method == http.MethodPost && p == "/projects":
		var in struct {
			Name string `json:"project_name"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		h.projects[in.Name] = int64(len(h.projects) + 1)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && parts[0] == "robots":
		w.Write([]byte(`[]`))
	case r.Method == http.MethodPost && p == "/robots":
		h.robots++
		writeJSON(w, map[string]any{"id": h.robots, "name": "robot$x+pull", "secret": "s3cret"})
	case r.Method == http.MethodGet && len(parts) == 6 && parts[2] == "repositories" && parts[4] == "artifacts":
		repo := strings.ReplaceAll(parts[3], "%252F", "/")
		digest, ok := h.tags[parts[1]+"/"+repo+":"+parts[5]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"digest": digest})
	default:
		http.Error(w, "fake harbor: no route "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

// fakeKube stores secrets and returns no Argo CD applications.
type fakeKube struct {
	mu      sync.Mutex
	secrets map[string]bool
}

func (k *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case strings.Contains(r.URL.Path, "/secrets/") && r.Method == http.MethodGet:
		if !k.secrets[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{}`))
	case strings.Contains(r.URL.Path, "/secrets/") && r.Method == http.MethodPatch:
		k.secrets[r.URL.Path] = true
		w.Write([]byte(`{}`))
	case strings.HasSuffix(r.URL.Path, "/applications"):
		w.Write([]byte(`{"items":[]}`))
	case strings.Contains(r.URL.Path, "/applications/"):
		http.NotFound(w, r)
	default:
		http.Error(w, "fake kube: no route", http.StatusNotImplemented)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// newKubeClient returns a client for a TLS test server.
func newKubeClient(t *testing.T, srv *httptest.Server) *kube.Client {
	t.Helper()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	kc, err := kube.New(kube.Config{Server: srv.URL, TokenFile: filepath.Join(dir, "token"), CAFile: filepath.Join(dir, "ca.crt")})
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
