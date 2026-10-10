package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"squared/internal/keycloak"
)

// fakeKeycloak has top-level groups with members and one permission of
// the admin-permissions client.
type fakeKeycloak struct {
	mu        sync.Mutex
	groups    map[string]string   // ID to name
	members   map[string][]string // group ID to user IDs
	resources []string            // resources of the permission
	puts      int
}

func newFakeKeycloak() *fakeKeycloak {
	return &fakeKeycloak{
		groups:    map[string]string{"id-admins": "squared-admins", "id-tenant": "tenant-acme-owner"},
		members:   map[string][]string{"id-tenant": {"u1"}},
		resources: []string{"id-admins"},
	}
}

func (k *fakeKeycloak) protected() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := append([]string(nil), k.resources...)
	sort.Strings(out)
	return out
}

func (k *fakeKeycloak) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
		writeJSON(w, map[string]any{"access_token": "t", "expires_in": 300})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/admin/realms/squared")
	const authz = "/clients/apc/authz/resource-server"
	switch {
	case p == "/clients" && r.URL.Query().Get("clientId") == "admin-permissions":
		writeJSON(w, []map[string]string{{"id": "apc"}})
	case strings.HasPrefix(p, "/group-by-path/"):
		name := strings.TrimPrefix(p, "/group-by-path/")
		for id, n := range k.groups {
			if n == name {
				writeJSON(w, map[string]string{"id": id, "name": n})
				return
			}
		}
		http.NotFound(w, r)
	case p == "/groups" && r.Method == http.MethodGet:
		var list []map[string]string
		for id, n := range k.groups {
			if n == r.URL.Query().Get("search") {
				list = append(list, map[string]string{"id": id, "name": n})
			}
		}
		writeJSON(w, list)
	case p == "/groups" && r.Method == http.MethodPost:
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		k.groups["id-"+in.Name] = in.Name
		w.WriteHeader(http.StatusCreated)
	case strings.HasSuffix(p, "/members"):
		id := strings.Split(p, "/")[2]
		var list []map[string]string
		if r.URL.Query().Get("first") == "0" {
			for _, u := range k.members[id] {
				list = append(list, map[string]string{"id": u})
			}
		}
		writeJSON(w, list)
	case p == authz+"/permission/scope" && r.URL.Query().Get("name") == "protected-groups":
		writeJSON(w, []map[string]string{{"id": "perm", "name": "protected-groups"}})
	case p == authz+"/permission/scope/perm" && r.Method == http.MethodGet:
		writeJSON(w, map[string]string{"id": "perm", "name": "protected-groups", "resourceType": "Groups", "decisionStrategy": "UNANIMOUS"})
	case p == authz+"/permission/scope/perm" && r.Method == http.MethodPut:
		var in keycloak.Permission
		json.NewDecoder(r.Body).Decode(&in)
		if !slices.Equal(in.Policies, []string{"protected-group-administrators"}) || len(in.Scopes) != 3 || in.ResourceType != "Groups" {
			http.Error(w, "the update lost the policies, the scopes or the type", http.StatusBadRequest)
			return
		}
		k.resources = in.Resources
		k.puts++
		w.WriteHeader(http.StatusCreated)
	case p == authz+"/permission/perm/resources":
		var list []map[string]string
		for _, id := range k.resources {
			list = append(list, map[string]string{"_id": "r-" + id, "name": id})
		}
		writeJSON(w, list)
	case p == authz+"/permission/perm/scopes":
		writeJSON(w, []map[string]string{{"name": "view"}, {"name": "manage"}, {"name": "manage-membership"}})
	case p == authz+"/permission/perm/associatedPolicies":
		writeJSON(w, []map[string]string{{"name": "protected-group-administrators"}})
	default:
		http.Error(w, "fake keycloak: no route "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// staticSource returns fixed references or an error.
type staticSource struct {
	refs []Reference
	err  error
}

func (s *staticSource) Name() string { return "static" }
func (s *staticSource) References(context.Context) ([]Reference, error) {
	return s.refs, s.err
}

func setup(t *testing.T, src *staticSource) (*Guard, *fakeKeycloak) {
	t.Helper()
	k := newFakeKeycloak()
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	kc := keycloak.New(srv.URL, "squared", "squared-guard", "secret", srv.Client())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(Config{Groups: []string{"/squared-admins"}}, []Source{src}, kc, log, nil), k
}

func TestGuardProtectsReferencedGroups(t *testing.T) {
	src := &staticSource{refs: []Reference{
		{Group: "/ops", Service: "Argo CD", Where: "rbac"},
		{Group: "/ops", Service: "Argo CD", Where: "rbac"},           // A duplicate is shown one time.
		{Group: "squared-admins", Service: "Argo CD", Where: "rbac"}, // short name: a warning
	}}
	g, k := setup(t, src)
	st, err := g.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The missing group /ops is made, then protected.
	if got := k.protected(); !slices.Equal(got, []string{"id-admins", "id-ops"}) {
		t.Fatalf("protected = %v", got)
	}
	if i := slices.IndexFunc(st.Protected, func(p ProtectedGroup) bool { return p.Path == "/ops" }); i < 0 || len(st.Protected[i].References) != 1 {
		t.Fatalf("references of /ops: %+v", st.Protected)
	}
	if !st.Changed || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "not a full group path") {
		t.Fatalf("status %+v", st)
	}

	// No change: no update.
	puts := k.puts
	if _, err := g.Once(context.Background()); err != nil || k.puts != puts {
		t.Fatalf("an unchanged configuration made an update (err %v)", err)
	}

	// A removed reference releases the group.
	src.refs = nil
	if _, err := g.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := k.protected(); !slices.Equal(got, []string{"id-admins"}) {
		t.Fatalf("protected after the removal = %v", got)
	}
}

func TestGuardWarnsAboutMembers(t *testing.T) {
	// A tenant group that a service gives rights to is protected, and the
	// guard warns because the group has members.
	src := &staticSource{refs: []Reference{{Group: "/tenant-acme-owner", Service: "Harbor", Where: "project platform"}}}
	g, k := setup(t, src)
	st, err := g.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(k.protected(), "id-tenant") {
		t.Fatal("the group is not protected")
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "has 1 members") {
		t.Fatalf("warnings %v", st.Warnings)
	}
}

func TestGuardKeepsProtectionWhenASourceFails(t *testing.T) {
	src := &staticSource{refs: []Reference{{Group: "/ops", Service: "Argo CD"}}}
	g, k := setup(t, src)
	if _, err := g.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.refs, src.err = nil, errors.New("connection refused")
	st, err := g.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := k.protected(); !slices.Equal(got, []string{"id-admins", "id-ops"}) {
		t.Fatalf("protection was removed after a source failure: %v", got)
	}
	if len(st.Errors) != 1 {
		t.Fatalf("errors %v", st.Errors)
	}
}

func TestGuardDoesNotMakeNestedGroups(t *testing.T) {
	src := &staticSource{refs: []Reference{{Group: "/a/b", Service: "Forgejo"}}}
	g, k := setup(t, src)
	st, _ := g.Once(context.Background())
	if len(st.Errors) != 1 || !strings.Contains(st.Errors[0], "only top-level groups") {
		t.Fatalf("errors %v", st.Errors)
	}
	if got := k.protected(); !slices.Equal(got, []string{"id-admins"}) {
		t.Fatalf("protected = %v", got)
	}
}
