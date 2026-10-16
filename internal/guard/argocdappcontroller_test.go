package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/gitops"
	"squared/internal/kube"
)

var appControllerSA = rbacSubject{"argocd", "argocd-application-controller"}

// devProjects are the projects of the development environment: the
// locked project default, the platform project and a tenant project.
func devProjects(t *testing.T) []kube.AppProject {
	t.Helper()
	var out []kube.AppProject
	for _, raw := range []string{
		`{"metadata": {"name": "default"}, "spec": {"sourceRepos": [], "destinations": [], "clusterResourceWhitelist": [], "namespaceResourceWhitelist": []}}`,
		`{"metadata": {"name": "squared-platform"}, "spec": {"sourceRepos": ["http://forgejo.localhost:30300/squared/platform.git"],
			"destinations": [{"server": "https://kubernetes.default.svc", "namespace": "*"}],
			"clusterResourceWhitelist": [{"group": "", "kind": "Namespace"}],
			"namespaceResourceWhitelist": [{"group": "", "kind": "ConfigMap"}, {"group": "", "kind": "ResourceQuota"}, {"group": "", "kind": "LimitRange"},
				{"group": "networking.k8s.io", "kind": "NetworkPolicy"}, {"group": "rbac.authorization.k8s.io", "kind": "RoleBinding"},
				{"group": "argoproj.io", "kind": "AppProject"}, {"group": "argoproj.io", "kind": "Application"}]}}`,
		string(projectJSON(t, renderedProject(t))),
	} {
		p := kube.AppProject{Raw: json.RawMessage(raw)}
		if err := json.Unmarshal(p.Raw, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// appControllerRBAC returns the RBAC of the cluster with a ClusterRole of
// the application controller with these rules.
func appControllerRBAC(t *testing.T, rules string) *kube.RBAC {
	t.Helper()
	r := chartRBACWith(t, nil, nil)
	var extra struct {
		Roles        []kube.Role
		ClusterRoles []kube.Role
		Bindings     []kube.RoleBinding
		Cluster      []kube.RoleBinding
	}
	if err := json.Unmarshal([]byte(`{
		"Roles": [{"metadata": {"name": "argocd-application-controller", "namespace": "argocd"}, "rules": [
			{"apiGroups": [""], "resources": ["secrets", "configmaps"], "verbs": ["get", "list", "watch"]},
			{"apiGroups": ["argoproj.io"], "resources": ["applications", "applicationsets", "appprojects"],
				"verbs": ["create", "get", "list", "watch", "update", "patch", "delete"]},
			{"apiGroups": [""], "resources": ["events"], "verbs": ["create", "list"]},
			{"apiGroups": ["apps"], "resources": ["deployments"], "verbs": ["get", "list", "watch"]}]}],
		"ClusterRoles": [{"metadata": {"name": "argocd-application-controller"}, "rules": `+rules+`}],
		"Bindings": [{"metadata": {"name": "argocd-application-controller", "namespace": "argocd"},
			"roleRef": {"kind": "Role", "name": "argocd-application-controller"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-application-controller", "namespace": "argocd"}]}],
		"Cluster": [{"metadata": {"name": "argocd-application-controller"},
			"roleRef": {"kind": "ClusterRole", "name": "argocd-application-controller"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-application-controller", "namespace": "argocd"}]}]}`), &extra); err != nil {
		t.Fatal(err)
	}
	r.Roles = append(r.Roles, extra.Roles...)
	r.ClusterRoles = append(r.ClusterRoles, extra.ClusterRoles...)
	r.RoleBindings = append(r.RoleBindings, extra.Bindings...)
	r.ClusterRoleBindings = append(r.ClusterRoleBindings, extra.Cluster...)
	return r
}

// devControllerRules are the rules of deploy/kind/argocd-values.yaml.
const devControllerRules = `[
	{"apiGroups": [""], "resources": ["namespaces", "resourcequotas", "limitranges", "configmaps", "serviceaccounts", "services"],
		"verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["networking.k8s.io"], "resources": ["networkpolicies", "ingresses"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["rolebindings"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["apps"], "resources": ["deployments"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["batch"], "resources": ["cronjobs"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["policy"], "resources": ["poddisruptionbudgets"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
	{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterroles"], "resourceNames": ["squared-worker-pull-secrets"], "verbs": ["bind"]},
	{"apiGroups": [""], "resources": ["pods", "endpoints", "events"], "verbs": ["get", "list", "watch"]},
	{"apiGroups": ["apps"], "resources": ["replicasets"], "verbs": ["get", "list", "watch"]},
	{"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["get", "list", "watch"]},
	{"apiGroups": ["discovery.k8s.io"], "resources": ["endpointslices"], "verbs": ["get", "list", "watch"]},
	{"apiGroups": ["argoproj.io"], "resources": ["applications", "appprojects"], "verbs": ["get", "list", "watch"]}]`

var bindRoles = []string{"squared-worker-pull-secrets"}

func TestCheckAppControllerRBAC(t *testing.T) {
	kinds, err := projectKinds(devProjects(t))
	if err != nil {
		t.Fatal(err)
	}
	if p := checkAppControllerRBAC(appControllerSA, kinds, bindRoles, appControllerRBAC(t, devControllerRules)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// Before the first tenant: the controller needs the kinds of the
	// tenant projects too.
	noTenant, err := projectKinds(devProjects(t)[:2])
	if err != nil {
		t.Fatal(err)
	}
	if p := checkAppControllerRBAC(appControllerSA, noTenant, bindRoles, appControllerRBAC(t, devControllerRules)); len(p) != 0 {
		t.Fatalf("without tenants: problems = %q", p)
	}

	// The default of the Helm chart.
	p := checkAppControllerRBAC(appControllerSA, kinds, bindRoles,
		appControllerRBAC(t, `[{"apiGroups": ["*"], "resources": ["*"], "verbs": ["*"]}, {"nonResourceURLs": ["*"], "verbs": ["*"]}]`))
	got := strings.Join(p, " | ")
	for _, want := range []string{
		"the ClusterRoleBinding argocd-application-controller (ClusterRole argocd-application-controller) gives the application controller * on * (*) in the cluster, more than the projects permit",
		"gives the application controller * on the URLs *, more than the projects permit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems = %q, want %q", p, want)
		}
	}
	// The open project default of Argo CD permits all kinds, thus all
	// write rights. But not read rights on Secrets and the verb escalate.
	open := append(devProjects(t), kube.AppProject{Raw: json.RawMessage(`{"spec": {"sourceRepos": ["*"],
		"destinations": [{"server": "*", "namespace": "*"}], "clusterResourceWhitelist": [{"group": "*", "kind": "*"}]}}`)})
	openKinds, err := projectKinds(open)
	if err != nil {
		t.Fatal(err)
	}
	p = checkAppControllerRBAC(appControllerSA, openKinds, bindRoles,
		appControllerRBAC(t, `[{"apiGroups": ["*"], "resources": ["*"], "verbs": ["create", "update", "patch", "delete"]}]`))
	if len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	tests := []struct {
		name, rules, want string
	}{
		{"Secrets", `[{"apiGroups": [""], "resources": ["secrets"], "verbs": ["get", "list"]}]`, "get list on secrets (core) in the cluster"},
		{"all kinds", `[{"apiGroups": [""], "resources": ["*"], "verbs": ["list"]}]`, "list on * (core)"},
		{"ClusterRoleBindings", `[{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterrolebindings"], "verbs": ["create"]}]`,
			"create on clusterrolebindings (rbac.authorization.k8s.io)"},
		{"Roles", `[{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["roles"], "verbs": ["create", "escalate"]}]`, "create escalate on roles"},
		{"bind all", `[{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterroles"], "verbs": ["bind"]}]`, "bind on clusterroles"},
		{"bind another", `[{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterroles"], "resourceNames": ["cluster-admin"], "verbs": ["bind"]}]`,
			"bind on clusterroles"},
		{"pods", `[{"apiGroups": [""], "resources": ["pods", "pods/exec"], "verbs": ["create"]}]`, "create on pods (core), create on pods/exec (core)"},
		{"impersonate", `[{"apiGroups": [""], "resources": ["serviceaccounts"], "verbs": ["impersonate"]}]`, "impersonate on serviceaccounts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkAppControllerRBAC(appControllerSA, kinds, bindRoles, appControllerRBAC(t, tt.rules))
			if !strings.Contains(strings.Join(p, " | "), tt.want) {
				t.Fatalf("problems = %q, want %q", p, tt.want)
			}
			for _, m := range p {
				if strings.Contains(m, ";") {
					t.Errorf("the message has a semicolon: %q", m)
				}
			}
		})
	}
}

func TestKindResource(t *testing.T) {
	for kind, want := range map[string]string{
		"Namespace": "namespaces", "NetworkPolicy": "networkpolicies", "Ingress": "ingresses", "PodDisruptionBudget": "poddisruptionbudgets",
		"ResourceQuota": "resourcequotas", "Gateway": "gateways", "Endpoints": "endpoints", "ConfigMap": "configmaps",
	} {
		if got := kindResource(kind); got != want {
			t.Errorf("kindResource(%q) = %q, want %q", kind, got, want)
		}
	}
	if !kindPermitted([]gitops.GroupKind{{Group: "apps", Kind: "*"}}, "apps", "deployments") ||
		kindPermitted([]gitops.GroupKind{{Group: "apps", Kind: "*"}}, "*", "deployments") ||
		kindPermitted([]gitops.GroupKind{{Group: "", Kind: "Service"}}, "", "*") {
		t.Error("kindPermitted is not correct for *")
	}
}
