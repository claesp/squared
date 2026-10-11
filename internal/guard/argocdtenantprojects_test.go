package guard

import (
	"encoding/json"
	"path"
	"strings"
	"testing"

	"squared/internal/domain"
	"squared/internal/gitops"
)

var devProjectRules = tenantProjectRules{
	ClusterServers: []string{"https://kubernetes.default.svc"},
	RepoPrefixes:   []string{"http://forgejo.localhost:30300/squared/"},
}

// renderedProject returns the AppProject that Squared makes for the
// tenant acme with the environments dev and prod, as a generic object.
func renderedProject(t *testing.T) map[string]any {
	t.Helper()
	c := gitops.Config{Org: "squared", RepoURLBase: "http://forgejo.localhost:30300", ClusterServer: "https://kubernetes.default.svc",
		ArgoNamespace: "argocd", GroupPrefix: "tenant-"}
	tenant := &domain.Tenant{Slug: "acme"}
	envs := []domain.Environment{{Name: "dev", Namespace: domain.Namespace("acme", "dev")}, {Name: "prod", Namespace: domain.Namespace("acme", "prod")}}
	files, err := c.TenantFiles(tenant, envs)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(files[path.Join(gitops.TenantDir("acme"), "appproject.json")], &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func projectJSON(t *testing.T, obj map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckTenantProjectRendered(t *testing.T) {
	problems, err := checkTenantProject("acme", projectJSON(t, renderedProject(t)), devProjectRules)
	if err != nil || len(problems) != 0 {
		t.Fatalf("the project that Squared makes has problems: %v, %v", problems, err)
	}
}

func TestCheckTenantProjectChanges(t *testing.T) {
	spec := func(o map[string]any) map[string]any { return o["spec"].(map[string]any) }
	tests := []struct {
		name   string
		change func(o map[string]any)
		want   string
	}{
		{"all namespaces", func(o map[string]any) {
			spec(o)["destinations"] = []any{map[string]any{"server": "https://kubernetes.default.svc", "namespace": "*"}}
		}, `the namespace "*"`},
		{"namespace of another tenant", func(o map[string]any) {
			spec(o)["destinations"] = []any{map[string]any{"server": "https://kubernetes.default.svc", "namespace": "acme-x-dev"}}
		}, `the namespace "acme-x-dev"`},
		{"system namespace", func(o map[string]any) {
			spec(o)["destinations"] = []any{map[string]any{"server": "https://kubernetes.default.svc", "namespace": "kube-system"}}
		}, `the namespace "kube-system"`},
		{"other cluster", func(o map[string]any) {
			spec(o)["destinations"] = []any{map[string]any{"server": "https://edge.example:6443", "namespace": "acme-dev"}}
		}, "the cluster https://edge.example:6443"},
		{"cluster by name", func(o map[string]any) {
			spec(o)["destinations"] = []any{map[string]any{"name": "in-cluster", "namespace": "acme-dev"}}
		}, `names the cluster "in-cluster"`},
		{"all repositories", func(o map[string]any) { spec(o)["sourceRepos"] = []any{"*"} }, `the source repository "*"`},
		{"cluster objects", func(o map[string]any) {
			spec(o)["clusterResourceWhitelist"] = []any{map[string]any{"group": "rbac.authorization.k8s.io", "kind": "ClusterRoleBinding"}}
		}, "cluster objects (rbac.authorization.k8s.io/ClusterRoleBinding)"},
		{"no kind list", func(o map[string]any) { delete(spec(o), "namespaceResourceWhitelist") }, "it permits all of them"},
		{"secrets", func(o map[string]any) {
			kinds := spec(o)["namespaceResourceWhitelist"].([]any)
			spec(o)["namespaceResourceWhitelist"] = append(kinds, map[string]any{"group": "", "kind": "Secret"})
		}, "the kinds Secret"},
		{"source namespaces", func(o map[string]any) { spec(o)["sourceNamespaces"] = []any{"acme-dev"} }, "applications in the namespaces acme-dev"},
		{"service accounts", func(o map[string]any) {
			spec(o)["destinationServiceAccounts"] = []any{map[string]any{"server": "*", "namespace": "*", "defaultServiceAccount": "admin"}}
		}, "destination service accounts"},
		{"sync for members", func(o map[string]any) {
			role := spec(o)["roles"].([]any)[0].(map[string]any)
			role["policies"] = append(role["policies"].([]any), "p, proj:tenant-acme:member, applications, sync, tenant-acme/*, allow")
		}, "applications, sync"},
		{"token of a role", func(o map[string]any) {
			role := spec(o)["roles"].([]any)[0].(map[string]any)
			role["jwtTokens"] = []any{map[string]any{"iat": 1}}
		}, "the role member has tokens"},
		{"another role", func(o map[string]any) {
			spec(o)["roles"] = append(spec(o)["roles"].([]any), map[string]any{"name": "deployer"})
		}, `the role "deployer"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := renderedProject(t)
			tt.change(o)
			problems, err := checkTenantProject("acme", projectJSON(t, o), devProjectRules)
			if err != nil || !strings.Contains(strings.Join(problems, " | "), tt.want) {
				t.Fatalf("problems = %q, err %v, want %q", problems, err, tt.want)
			}
		})
	}
}
