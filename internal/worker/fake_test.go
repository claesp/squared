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
	users   map[string]string // user name to email
	teams   map[int64]*fakeTeam
}

type fakeTeam struct {
	name    string
	repos   map[string]bool
	members map[string]bool
}

func (f *fakeForgejo) team(name string) *fakeTeam {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.teams {
		if t.name == name {
			return t
		}
	}
	return nil
}

type fakeCommit struct {
	sha   string
	files map[string][]byte
	msg   string
}

func newFakeForgejo() *fakeForgejo {
	return &fakeForgejo{repos: map[string][]fakeCommit{}, users: map[string]string{}, teams: map[int64]*fakeTeam{}}
}

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
	if f.serveTeams(w, r, parts) {
		return
	}
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
	members  map[string][]harborMember
	memberID int64
}

func (h *fakeHarbor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v2.0")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if h.serveMembers(w, r, parts) {
		return
	}
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

// serveTeams answers the requests for teams and users. It returns false
// for other requests.
func (f *fakeForgejo) serveTeams(w http.ResponseWriter, r *http.Request, parts []string) bool {
	switch {
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "teams":
		var list []map[string]any
		for id, t := range f.teams {
			list = append(list, map[string]any{"id": id, "name": t.name})
		}
		writeJSON(w, list)
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "teams":
		var in struct {
			Name       string
			Permission string
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Permission != "read" {
			http.Error(w, "the fake accepts only read teams", http.StatusBadRequest)
			return true
		}
		id := int64(len(f.teams) + 1)
		f.teams[id] = &fakeTeam{name: in.Name, repos: map[string]bool{}, members: map[string]bool{}}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"id": id, "name": in.Name})
	case parts[0] == "teams" && len(parts) >= 2:
		var id int64
		fmt.Sscan(parts[1], &id)
		t, ok := f.teams[id]
		if !ok {
			http.NotFound(w, r)
			return true
		}
		switch {
		case r.Method == http.MethodDelete && len(parts) == 2:
			delete(f.teams, id)
		case r.Method == http.MethodPut && len(parts) == 5 && parts[2] == "repos":
			t.repos[parts[3]+"/"+parts[4]] = true
		case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "members":
			var list []map[string]string
			if r.URL.Query().Get("page") == "1" {
				for _, name := range sortedKeys(t.members) {
					list = append(list, map[string]string{"login": name})
				}
			}
			writeJSON(w, list)
		case r.Method == http.MethodPut && len(parts) == 4 && parts[2] == "members":
			if _, ok := f.users[parts[3]]; !ok {
				http.NotFound(w, r)
				return true
			}
			t.members[parts[3]] = true
		case r.Method == http.MethodDelete && len(parts) == 4 && parts[2] == "members":
			delete(t.members, parts[3])
		default:
			return false
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "users":
		email, ok := f.users[parts[1]]
		if !ok {
			http.NotFound(w, r)
			return true
		}
		writeJSON(w, map[string]string{"login": parts[1], "email": email})
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "admin" && parts[1] == "users":
		var in struct{ Username, Email, Password string }
		json.NewDecoder(r.Body).Decode(&in)
		if in.Password == "" {
			http.Error(w, "password required", http.StatusUnprocessableEntity)
			return true
		}
		if in.Username == "admin" {
			http.Error(w, "name is reserved", http.StatusUnprocessableEntity)
			return true
		}
		f.users[in.Username] = in.Email
		w.WriteHeader(http.StatusCreated)
	default:
		return false
	}
	return true
}

// harborMember is a member of a fake Harbor project.
type harborMember struct {
	ID         int64  `json:"id"`
	EntityName string `json:"entity_name"`
	EntityType string `json:"entity_type"`
	RoleID     int    `json:"role_id"`
}

// serveMembers answers the requests for project members.
func (h *fakeHarbor) serveMembers(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if len(parts) < 3 || parts[0] != "projects" || parts[2] != "members" {
		return false
	}
	project := parts[1]
	if h.members == nil {
		h.members = map[string][]harborMember{}
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 3:
		list := h.members[project]
		if list == nil {
			list = []harborMember{}
		}
		writeJSON(w, list)
	case r.Method == http.MethodPost && len(parts) == 3:
		var in struct {
			RoleID      int `json:"role_id"`
			MemberGroup struct {
				GroupName string `json:"group_name"`
				GroupType int    `json:"group_type"`
			} `json:"member_group"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.MemberGroup.GroupType != 3 {
			http.Error(w, "the fake accepts only OIDC groups", http.StatusBadRequest)
			return true
		}
		h.memberID++
		h.members[project] = append(h.members[project], harborMember{ID: h.memberID, EntityName: in.MemberGroup.GroupName, EntityType: "g", RoleID: in.RoleID})
		w.WriteHeader(http.StatusCreated)
	case len(parts) == 4:
		var id int64
		fmt.Sscan(parts[3], &id)
		list := h.members[project]
		for i := range list {
			if list[i].ID != id {
				continue
			}
			if r.Method == http.MethodDelete {
				h.members[project] = append(list[:i], list[i+1:]...)
			} else {
				var in struct {
					RoleID int `json:"role_id"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				list[i].RoleID = in.RoleID
			}
			return true
		}
		http.NotFound(w, r)
	default:
		return false
	}
	return true
}

// roles returns the role of each group member of a project.
func (h *fakeHarbor) roles(project string) map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for _, m := range h.members[project] {
		out[m.EntityName] = m.RoleID
	}
	return out
}

// fakeKeycloak has groups and users of one realm.
type fakeKeycloak struct {
	mu      sync.Mutex
	users   map[string]string          // email to user ID
	groups  map[string]string          // group ID to name
	members map[string]map[string]bool // group ID to user IDs
}

func newFakeKeycloak(users map[string]string) *fakeKeycloak {
	return &fakeKeycloak{users: users, groups: map[string]string{}, members: map[string]map[string]bool{}}
}

// groupMembers returns the user IDs of the group with a name, or nil if
// the group does not exist.
func (k *fakeKeycloak) groupMembers(name string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, n := range k.groups {
		if n == name {
			return append([]string{}, sortedKeys(k.members[id])...)
		}
	}
	return nil
}

func (k *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if r.URL.Path == "/realms/squared/protocol/openid-connect/token" {
		if id, secret, _ := r.BasicAuth(); id != "squared-worker" || secret != "worker-secret" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"access_token": "admin-token", "expires_in": 300})
		return
	}
	if r.Header.Get("Authorization") != "Bearer admin-token" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/admin/realms/squared/"), "/")
	switch {
	case parts[0] == "groups" && len(parts) == 1 && r.Method == http.MethodGet:
		var list []map[string]string
		for id, name := range k.groups {
			if name == r.URL.Query().Get("search") {
				list = append(list, map[string]string{"id": id, "name": name})
			}
		}
		writeJSON(w, list)
	case parts[0] == "groups" && len(parts) == 1 && r.Method == http.MethodPost:
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		id := "g-" + in.Name
		k.groups[id] = in.Name
		k.members[id] = map[string]bool{}
		w.WriteHeader(http.StatusCreated)
	case parts[0] == "groups" && len(parts) == 2 && r.Method == http.MethodDelete:
		delete(k.groups, parts[1])
		delete(k.members, parts[1])
		w.WriteHeader(http.StatusNoContent)
	case parts[0] == "groups" && len(parts) == 3 && parts[2] == "members":
		var list []map[string]string
		if r.URL.Query().Get("first") == "0" {
			for _, id := range sortedKeys(k.members[parts[1]]) {
				list = append(list, map[string]string{"id": id})
			}
		}
		writeJSON(w, list)
	case parts[0] == "users" && len(parts) == 1:
		var list []map[string]string
		if id, ok := k.users[r.URL.Query().Get("email")]; ok {
			list = append(list, map[string]string{"id": id, "email": r.URL.Query().Get("email")})
		}
		writeJSON(w, list)
	case parts[0] == "users" && len(parts) == 4 && parts[2] == "groups":
		m, ok := k.members[parts[3]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			m[parts[1]] = true
		} else {
			delete(m, parts[1])
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "fake keycloak: no route "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}
