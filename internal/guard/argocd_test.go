package guard

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"squared/internal/kube"
	"squared/internal/store/storetest"
)

func TestCheckArgoRBAC(t *testing.T) {
	data := map[string]string{
		"policy.csv":       "g, /squared-admins, role:admin\np, role:x, applications, get, */*, allow",
		"policy.ops.csv":   "g, /ops, role:readonly\ng, ops, role:readonly\ng, someone@example.com, role:admin",
		"policy.default":   "",
		"policy.matchMode": "glob",
		"scopes":           "[groups, email]",
	}
	refs, warnings, err := checkArgoRBAC(data, nil)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	// The short name is returned: the guard reports it.
	if got := groupsOf(refs); !slices.Equal(got, []string{"/ops", "/squared-admins", "ops"}) {
		t.Fatalf("groups = %q", got)
	}
	if refs[0].Where != "policy.csv" {
		t.Fatalf("where = %q", refs[0].Where)
	}
}

func TestCheckArgoRBACChanges(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"policy.csv": "g, /squared-admins, role:admin", "scopes": "[groups]"}
	}
	tests := []struct {
		name   string
		change func(map[string]string)
		ignore []string
		errMsg string
	}{
		{"new key", func(d map[string]string) { d["policy.groups"] = "/x" }, nil, "new key policy.groups"},
		{"accepted key", func(d map[string]string) { d["policy.groups"] = "/x" }, []string{"policy.groups"}, ""},
		{"unknown claim", func(d map[string]string) { d["scopes"] = "[groups, roles]" }, nil, "claim roles"},
		{"block list", func(d map[string]string) { d["scopes"] = "- groups\n- 'email'\n" }, nil, ""},
		{"no list", func(d map[string]string) { d["scopes"] = "groups" }, nil, "not a list"},
		{"no scopes", func(d map[string]string) { delete(d, "scopes") }, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base()
			tt.change(d)
			refs, _, err := checkArgoRBAC(d, tt.ignore)
			if tt.errMsg == "" && err != nil || tt.errMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.errMsg)) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
			// The groups stay, also with an error.
			if got := groupsOf(refs); !slices.Equal(got, []string{"/squared-admins"}) {
				t.Fatalf("groups = %q", got)
			}
		})
	}
}

func TestCheckArgoRBACWithoutGroupsClaim(t *testing.T) {
	_, warnings, err := checkArgoRBAC(map[string]string{"policy.csv": "g, /a, role:admin", "scopes": "[email]"}, nil)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "no groups claim") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

func TestCheckArgoProjectFields(t *testing.T) {
	raw := []byte(`{"metadata": {"name": "p"}, "spec": {"sourceRepos": ["*"], "destinations": [],
		"roles": [{"name": "member", "groups": ["/a"], "policies": ["p, proj:p:member, applications, get, p/*, allow"]}]}}`)
	if w, p, err := checkArgoProjectFields(raw, nil); err != nil || len(w) != 0 || len(p) != 0 {
		t.Fatalf("warnings %v, problems %v, err %v", w, p, err)
	}

	raw = []byte(`{"spec": {"roles": [{"name": "member", "subjectGroups": ["/a"]}, {"name": "x", "subjectGroups": []}],
		"teamAccess": {}, "uiColor": "blue"}}`)
	w, p, err := checkArgoProjectFields(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"new field spec.teamAccess can give rights", "new field roles.subjectGroups can give rights"}; !slices.Equal(p, want) {
		t.Fatalf("problems = %q", p)
	}
	if len(w) != 1 || !strings.Contains(w[0], "spec.uiColor") {
		t.Fatalf("warnings = %q", w)
	}
	if _, p, _ := checkArgoProjectFields(raw, []string{"spec.teamAccess", "roles.subjectGroups"}); len(p) != 0 {
		t.Fatalf("ignoreFields did not apply: %q", p)
	}
}

// fakeKube returns a Kubernetes client for a server that answers with the
// given objects for each path.
func fakeKube(t *testing.T, objects map[string]string) *kube.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	storetest.Must(t, os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600))
	storetest.Must(t, os.WriteFile(filepath.Join(dir, "token"), []byte("token"), 0o600))
	kc, err := kube.New(kube.Config{Server: srv.URL, CAFile: filepath.Join(dir, "ca.crt"), TokenFile: filepath.Join(dir, "token")})
	storetest.Must(t, err)
	return kc
}

func TestArgoProjectsSource(t *testing.T) {
	kc := fakeKube(t, map[string]string{"/apis/argoproj.io/v1alpha1/namespaces/argocd/appprojects": `{"items": [
		{"metadata": {"name": "tenant-shop", "labels": {"app.kubernetes.io/managed-by": "squared"}},
		 "spec": {"roles": [{"name": "member", "groups": ["/tenant-shop-owner", "/ops"],
		   "policies": ["p, proj:tenant-shop:member, applications, get, tenant-shop/*, allow", "g, /hidden, role:admin"]}]}},
		{"metadata": {"name": "default"}, "spec": {"sourceRepos": ["*"], "roles": [{"name": "r", "groups": ["/tenant-shop-owner"], "newGroups": []}]}}]}`})
	src := &argoProjectsSource{sc: SourceConfig{Service: "Argo CD", Namespace: "argocd"}, kube: kc, tenants: tenantGroups{prefix: "tenant-"}}
	refs, _, err := src.References(context.Background())
	if err == nil || !strings.Contains(err.Error(), "project default: new field roles.newGroups can give rights") {
		t.Fatalf("err = %v", err)
	}
	// The tenant group in its own member role is expected. The same group in
	// another project, and a "g" line in the policies, are protected.
	if got := groupsOf(refs); !slices.Equal(got, []string{"/hidden", "/ops", "/tenant-shop-owner"}) {
		t.Fatalf("groups = %q", got)
	}
	for _, r := range refs {
		if r.Group == "/hidden" && r.Where != "project tenant-shop role member policies" {
			t.Fatalf("where = %q", r.Where)
		}
	}
}
