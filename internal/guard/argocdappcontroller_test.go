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
		{"pods/exec", `[{"apiGroups": [""], "resources": ["pods", "pods/exec"], "verbs": ["create"]}]`, "gives the application controller create on pods/exec (core) in the cluster"},
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

// serverRBAC returns the RBAC of the cluster with the Role of the API
// server of the Helm chart and a ClusterRole with these rules.
func serverRBAC(t *testing.T, rules string) *kube.RBAC {
	t.Helper()
	r := chartRBACWith(t, nil, nil)
	var extra struct {
		Roles, ClusterRoles []kube.Role
		Bindings, Cluster   []kube.RoleBinding
	}
	if err := json.Unmarshal([]byte(`{
		"Roles": [{"metadata": {"name": "argocd-server", "namespace": "argocd"}, "rules": [
			{"apiGroups": [""], "resources": ["secrets", "configmaps"], "verbs": ["create", "get", "list", "watch", "update", "patch", "delete"]},
			{"apiGroups": ["argoproj.io"], "resources": ["applications", "applicationsets", "appprojects"],
				"verbs": ["create", "get", "list", "watch", "update", "delete", "patch"]},
			{"apiGroups": [""], "resources": ["events"], "verbs": ["create", "list"]}]}],
		"ClusterRoles": [{"metadata": {"name": "argocd-server"}, "rules": `+rules+`}],
		"Bindings": [{"metadata": {"name": "argocd-server", "namespace": "argocd"}, "roleRef": {"kind": "Role", "name": "argocd-server"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-server", "namespace": "argocd"}]}],
		"Cluster": [{"metadata": {"name": "argocd-server"}, "roleRef": {"kind": "ClusterRole", "name": "argocd-server"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-server", "namespace": "argocd"}]}]}`), &extra); err != nil {
		t.Fatal(err)
	}
	r.Roles = append(r.Roles, extra.Roles...)
	r.ClusterRoles = append(r.ClusterRoles, extra.ClusterRoles...)
	r.RoleBindings = append(r.RoleBindings, extra.Bindings...)
	r.ClusterRoleBindings = append(r.ClusterRoleBindings, extra.Cluster...)
	return r
}

// devServerRules are the rules of deploy/kind/argocd-values.yaml.
const devServerRules = `[
	{"apiGroups": [""], "resources": ["namespaces", "resourcequotas", "limitranges", "configmaps", "serviceaccounts", "services"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": ["networking.k8s.io"], "resources": ["networkpolicies", "ingresses"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["rolebindings"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": ["apps"], "resources": ["deployments"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": ["batch"], "resources": ["cronjobs"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": ["policy"], "resources": ["poddisruptionbudgets"], "verbs": ["get", "patch", "delete"]},
	{"apiGroups": [""], "resources": ["pods", "pods/log", "endpoints"], "verbs": ["get"]},
	{"apiGroups": [""], "resources": ["events"], "verbs": ["list"]},
	{"apiGroups": ["apps"], "resources": ["replicasets"], "verbs": ["get"]},
	{"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["get"]},
	{"apiGroups": ["discovery.k8s.io"], "resources": ["endpointslices"], "verbs": ["get"]}]`

var serverSA = rbacSubject{"argocd", "argocd-server"}

func TestCheckServerRBAC(t *testing.T) {
	kinds, err := projectKinds(devProjects(t))
	if err != nil {
		t.Fatal(err)
	}
	if p := checkServerRBAC(serverSA, kinds, serverRBAC(t, devServerRules)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	// The ClusterRole of the Helm chart.
	p := checkServerRBAC(serverSA, kinds, serverRBAC(t, `[
		{"apiGroups": ["*"], "resources": ["*"], "verbs": ["delete", "get", "patch"]},
		{"apiGroups": [""], "resources": ["events"], "verbs": ["list", "create"]},
		{"apiGroups": [""], "resources": ["pods", "pods/log"], "verbs": ["get"]},
		{"apiGroups": ["argoproj.io"], "resources": ["applications", "applicationsets"], "verbs": ["get", "list", "update", "watch"]},
		{"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["create"]},
		{"apiGroups": ["argoproj.io"], "resources": ["workflows"], "verbs": ["create"]}]`))
	got := strings.Join(p, " | ")
	for _, want := range []string{
		"the ClusterRoleBinding argocd-server (ClusterRole argocd-server) gives the API server delete get patch on * (*) in the cluster, more than the projects permit",
		"gives the API server create on events (core) in the cluster",
		"gives the API server update on applications (argoproj.io), update on applicationsets (argoproj.io) in the cluster",
		"gives the API server create on jobs (batch) in the cluster",
		"gives the API server create on workflows (argoproj.io) in the cluster",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems = %q, want %q", p, want)
		}
	}
	// The server does not create the objects of the applications.
	p = checkServerRBAC(serverSA, kinds, serverRBAC(t, `[{"apiGroups": ["apps"], "resources": ["deployments"], "verbs": ["create", "update"]}]`))
	if !strings.Contains(strings.Join(p, " | "), "create update on deployments (apps)") {
		t.Fatalf("problems = %q", p)
	}
}

var repoSA = rbacSubject{"argocd", "argocd-repo-server"}

func TestCheckRepoServer(t *testing.T) {
	// The Helm chart: a Role without rules, and a token.
	r := chartRBACWith(t, []kube.RoleBinding{binding(t, `{"metadata": {"name": "argocd-repo-server", "namespace": "argocd"},
		"roleRef": {"kind": "Role", "name": "argocd-repo-server"},
		"subjects": [{"kind": "ServiceAccount", "name": "argocd-repo-server", "namespace": "argocd"}]}`)}, nil)
	r.Roles = append(r.Roles, kube.Role{})
	r.Roles[len(r.Roles)-1].Metadata.Name, r.Roles[len(r.Roles)-1].Metadata.Namespace = "argocd-repo-server", "argocd"
	if p := checkRepoServer(repoSA, &kube.WorkloadToken{ServiceAccount: "argocd-repo-server"}, r); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	p := checkRepoServer(repoSA, &kube.WorkloadToken{ServiceAccount: "argocd-repo-server",
		Mounted: "automountServiceAccountToken of the service account argocd-repo-server"}, r)
	if len(p) != 1 || !strings.Contains(p[0], "its pods get a token of the Kubernetes API (automountServiceAccountToken of the service account argocd-repo-server), which it does not need") {
		t.Fatalf("problems = %q", p)
	}

	// Each right is more than it needs, also a read right in its namespace.
	reads := binding(t, `{"metadata": {"name": "reads", "namespace": "argocd"}, "roleRef": {"kind": "ClusterRole", "name": "view"},
		"subjects": [{"kind": "Group", "name": "system:serviceaccounts:argocd"}]}`)
	p = checkRepoServer(repoSA, &kube.WorkloadToken{ServiceAccount: "argocd-repo-server"}, chartRBACWith(t, []kube.RoleBinding{reads}, nil))
	if len(p) != 1 || !strings.Contains(p[0], "the RoleBinding argocd/reads (ClusterRole view) gives the repository server get list watch on pods (core), get list watch on configmaps (core) in the namespace argocd, more than it needs") {
		t.Fatalf("problems = %q", p)
	}
	for _, m := range p {
		if strings.Contains(m, ";") {
			t.Errorf("the message has a semicolon: %q", m)
		}
	}
}

func TestCheckRedis(t *testing.T) {
	r := chartRBACWith(t, nil, nil)
	if p := checkRedis(rbacSubject{"argocd", "argocd-redis"}, &kube.WorkloadToken{ServiceAccount: "argocd-redis"}, r); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// The Helm chart: the account default and a token.
	p := checkRedis(rbacSubject{"argocd", "default"}, &kube.WorkloadToken{ServiceAccount: "default",
		Mounted: "automountServiceAccountToken of the pod template"}, r)
	got := strings.Join(p, " | ")
	for _, want := range []string{
		"its pods get a token of the Kubernetes API (automountServiceAccountToken of the pod template), which it does not need",
		"it has the service account default of the namespace",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems = %q, want %q", p, want)
		}
	}
	// A right through the group of all service accounts.
	reads := binding(t, `{"metadata": {"name": "reads"}, "roleRef": {"kind": "ClusterRole", "name": "view"},
		"subjects": [{"kind": "Group", "name": "system:serviceaccounts"}]}`)
	p = checkRedis(rbacSubject{"argocd", "argocd-redis"}, &kube.WorkloadToken{ServiceAccount: "argocd-redis"}, chartRBACWith(t, nil, []kube.RoleBinding{reads}))
	if len(p) != 1 || !strings.Contains(p[0], "the ClusterRoleBinding reads (ClusterRole view) gives Redis get list watch on pods (core)") {
		t.Fatalf("problems = %q", p)
	}
}

func TestCheckRedisSecretInit(t *testing.T) {
	sa := rbacSubject{"argocd", "argocd-redis-secret-init"}
	// Without the job.
	if p, w := checkRedisSecretInit(sa, chartRBACWith(t, nil, nil)); len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	// The Role of the Helm chart.
	withRole := func(rules string) *kube.RBAC {
		r := chartRBACWith(t, []kube.RoleBinding{binding(t, `{"metadata": {"name": "argocd-redis-secret-init", "namespace": "argocd"},
			"roleRef": {"kind": "Role", "name": "argocd-redis-secret-init"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-redis-secret-init", "namespace": "argocd"}]}`)}, nil)
		var role kube.Role
		if err := json.Unmarshal([]byte(`{"metadata": {"name": "argocd-redis-secret-init", "namespace": "argocd"}, "rules": `+rules+`}`), &role); err != nil {
			t.Fatal(err)
		}
		r.Roles = append(r.Roles, role)
		return r
	}
	chart := `[{"apiGroups": [""], "resourceNames": ["argocd-redis"], "resources": ["secrets"], "verbs": ["get"]},
		{"apiGroups": [""], "resources": ["secrets"], "verbs": ["create"]}]`
	p, w := checkRedisSecretInit(sa, withRole(chart))
	if len(p) != 0 || len(w) != 1 || !strings.Contains(w[0], "the service account argocd-redis-secret-init of the Helm chart can make Secrets in the namespace argocd") {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	// More than the job needs: read all Secrets.
	p, _ = checkRedisSecretInit(sa, withRole(`[{"apiGroups": [""], "resources": ["secrets"], "verbs": ["get", "list", "create"]}]`))
	if len(p) != 1 || !strings.Contains(p[0], "gives the service account argocd-redis-secret-init get list on secrets (core) in the namespace argocd, more than the job needs") {
		t.Fatalf("problems = %q", p)
	}
}

func TestCheckDex(t *testing.T) {
	sa := rbacSubject{"argocd", "argocd-dex-server"}
	withDex := func(rules string) *kube.RBAC {
		r := chartRBACWith(t, []kube.RoleBinding{binding(t, `{"metadata": {"name": "argocd-dex-server", "namespace": "argocd"},
			"roleRef": {"kind": "Role", "name": "argocd-dex-server"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-dex-server", "namespace": "argocd"}]}`)}, nil)
		var role kube.Role
		if err := json.Unmarshal([]byte(`{"metadata": {"name": "argocd-dex-server", "namespace": "argocd"}, "rules": `+rules+`}`), &role); err != nil {
			t.Fatal(err)
		}
		r.Roles = append(r.Roles, role)
		return r
	}
	chart := `[{"apiGroups": [""], "resources": ["secrets", "configmaps"], "verbs": ["get", "list", "watch"]}]`

	// Off, as in the development environment.
	if p, w := checkDex(sa, false, false, chartRBACWith(t, nil, nil)); len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	// On, with a Dex configuration.
	if p, w := checkDex(sa, true, true, withDex(chart)); len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	// On without work: the default of the chart with oidc.config.
	p, w := checkDex(sa, true, false, withDex(chart))
	if len(p) != 0 || len(w) != 1 || !strings.Contains(w[0], "Dex runs, but argocd-cm has no dex.config, thus Argo CD does not use it") {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	// More rights: a binding stays also without the Deployment.
	p, _ = checkDex(sa, false, false, withDex(`[{"apiGroups": [""], "resources": ["secrets"], "verbs": ["get", "list", "create"]}]`))
	if len(p) != 1 || !strings.Contains(p[0], "the RoleBinding argocd/argocd-dex-server (Role argocd-dex-server) gives Dex create on secrets (core) in the namespace argocd, more than Dex needs") {
		t.Fatalf("problems = %q", p)
	}
}
