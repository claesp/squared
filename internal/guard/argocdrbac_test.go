package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/kube"
)

// chartRBAC is the RBAC of a Kind cluster with Argo CD: the Role and the
// RoleBinding of the ApplicationSet controller of the Helm chart 10, and
// the bindings of Kubernetes for each user.
const chartRBAC = `{
	"Roles": [{"metadata": {"name": "argocd-applicationset-controller", "namespace": "argocd"}, "rules": [
		{"apiGroups": ["argoproj.io"], "resources": ["applications", "applicationsets", "applicationsets/finalizers"],
			"verbs": ["create", "delete", "get", "list", "patch", "update", "watch"]},
		{"apiGroups": ["argoproj.io"], "resources": ["applicationsets/status"], "verbs": ["get", "patch", "update"]},
		{"apiGroups": ["argoproj.io"], "resources": ["appprojects"], "verbs": ["get", "list", "watch"]},
		{"apiGroups": [""], "resources": ["events"], "verbs": ["create", "get", "list", "patch", "watch"]},
		{"apiGroups": [""], "resources": ["configmaps"], "verbs": ["create", "update", "delete", "get", "list", "patch", "watch"]},
		{"apiGroups": [""], "resources": ["secrets"], "verbs": ["get", "list", "watch"]},
		{"apiGroups": ["apps", "extensions"], "resources": ["deployments"], "verbs": ["get", "list", "watch"]},
		{"apiGroups": ["coordination.k8s.io"], "resources": ["leases"], "verbs": ["create"]},
		{"apiGroups": ["coordination.k8s.io"], "resourceNames": ["58ac56fa.applicationsets.argoproj.io"], "resources": ["leases"],
			"verbs": ["get", "update", "create"]}]},
		{"metadata": {"name": "kube-proxy", "namespace": "kube-system"}, "rules": [
			{"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get"]}]}],
	"ClusterRoles": [
		{"metadata": {"name": "system:basic-user"}, "rules": [
			{"apiGroups": ["authorization.k8s.io"], "resources": ["selfsubjectaccessreviews", "selfsubjectrulesreviews"], "verbs": ["create"]},
			{"apiGroups": ["authentication.k8s.io"], "resources": ["selfsubjectreviews"], "verbs": ["create"]}]},
		{"metadata": {"name": "system:discovery"}, "rules": [{"nonResourceURLs": ["/api", "/api/*", "/version"], "verbs": ["get"]}]},
		{"metadata": {"name": "system:cluster-trust-bundle-discovery"}, "rules": [
			{"apiGroups": ["certificates.k8s.io"], "resources": ["clustertrustbundles"], "verbs": ["get", "list", "watch"]}]},
		{"metadata": {"name": "cluster-admin"}, "rules": [{"apiGroups": ["*"], "resources": ["*"], "verbs": ["*"]}]},
		{"metadata": {"name": "view"}, "rules": [{"apiGroups": [""], "resources": ["pods", "configmaps"], "verbs": ["get", "list", "watch"]}]},
		{"metadata": {"name": "placements"}, "rules": [
			{"apiGroups": ["cluster.open-cluster-management.io"], "resources": ["placementdecisions"], "verbs": ["get", "list", "watch"]}]}],
	"RoleBindings": [
		{"metadata": {"name": "argocd-applicationset-controller", "namespace": "argocd"},
			"roleRef": {"kind": "Role", "name": "argocd-applicationset-controller"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "argocd"}]},
		{"metadata": {"name": "kube-proxy", "namespace": "kube-system"}, "roleRef": {"kind": "Role", "name": "kube-proxy"},
			"subjects": [{"kind": "Group", "name": "system:bootstrappers:kubeadm:default-node-token"}]}],
	"ClusterRoleBindings": [
		{"metadata": {"name": "system:basic-user"}, "roleRef": {"kind": "ClusterRole", "name": "system:basic-user"},
			"subjects": [{"kind": "Group", "name": "system:authenticated"}]},
		{"metadata": {"name": "system:discovery"}, "roleRef": {"kind": "ClusterRole", "name": "system:discovery"},
			"subjects": [{"kind": "Group", "name": "system:authenticated"}]},
		{"metadata": {"name": "system:cluster-trust-bundle-discovery"}, "roleRef": {"kind": "ClusterRole", "name": "system:cluster-trust-bundle-discovery"},
			"subjects": [{"kind": "Group", "name": "system:authenticated"}]},
		{"metadata": {"name": "cluster-admin"}, "roleRef": {"kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": [{"kind": "Group", "name": "system:masters"}]}]
}`

var appSetSA = rbacSubject{"argocd", "argocd-applicationset-controller"}

func chartRBACWith(t *testing.T, rbs, crbs []kube.RoleBinding) *kube.RBAC {
	t.Helper()
	var r kube.RBAC
	if err := json.Unmarshal([]byte(chartRBAC), &r); err != nil {
		t.Fatal(err)
	}
	r.RoleBindings = append(r.RoleBindings, rbs...)
	r.ClusterRoleBindings = append(r.ClusterRoleBindings, crbs...)
	return &r
}

func binding(t *testing.T, s string) kube.RoleBinding {
	t.Helper()
	var b kube.RoleBinding
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckAppSetControllerRBAC(t *testing.T) {
	if p := checkAppSetControllerRBAC(appSetSA, nil, chartRBACWith(t, nil, nil)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// Read rights in the namespace of Argo CD, for a cluster decision
	// resource.
	reads := binding(t, `{"metadata": {"name": "placements", "namespace": "argocd"}, "roleRef": {"kind": "ClusterRole", "name": "placements"},
		"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "argocd"}]}`)
	if p := checkAppSetControllerRBAC(appSetSA, nil, chartRBACWith(t, []kube.RoleBinding{reads}, nil)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// The rights for the ApplicationSets in an application namespace.
	team := binding(t, `{"metadata": {"name": "appsets", "namespace": "team-a"}, "roleRef": {"kind": "Role", "name": "argocd-applicationset-controller"},
		"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "argocd"}]}`)
	r := chartRBACWith(t, []kube.RoleBinding{team}, nil)
	teamRole := r.Roles[0]
	teamRole.Metadata.Namespace = "team-a"
	teamRole.Rules = teamRole.Rules[:4]
	r.Roles = append(r.Roles, teamRole)
	if p := checkAppSetControllerRBAC(appSetSA, []string{"team-a"}, r); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	tests := []struct {
		name          string
		rbs, crbs     []kube.RoleBinding
		appNamespaces []string
		want          string
	}{
		{"cluster-admin", nil, []kube.RoleBinding{binding(t, `{"metadata": {"name": "appset-admin"}, "roleRef": {"kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "argocd"}]}`)}, nil,
			"the ClusterRoleBinding appset-admin (ClusterRole cluster-admin) gives the ApplicationSet controller * on * (*) in the cluster, more than Argo CD needs"},
		{"read in the cluster", nil, []kube.RoleBinding{binding(t, `{"metadata": {"name": "viewers"}, "roleRef": {"kind": "ClusterRole", "name": "view"},
			"subjects": [{"kind": "Group", "name": "system:serviceaccounts:argocd"}]}`)}, nil,
			"the ClusterRoleBinding viewers (ClusterRole view) gives the ApplicationSet controller get list watch on pods (core), get list watch on configmaps (core) in the cluster"},
		{"read in a tenant namespace", []kube.RoleBinding{binding(t, `{"metadata": {"name": "view", "namespace": "acme-dev"}, "roleRef": {"kind": "ClusterRole", "name": "view"},
			"subjects": [{"kind": "User", "name": "system:serviceaccount:argocd:argocd-applicationset-controller"}]}`)}, nil, nil,
			"the RoleBinding acme-dev/view (ClusterRole view) gives the ApplicationSet controller get list watch on pods (core), get list watch on configmaps (core) in the namespace acme-dev"},
		{"write in the namespace of Argo CD", []kube.RoleBinding{binding(t, `{"metadata": {"name": "admin", "namespace": "argocd"}, "roleRef": {"kind": "ClusterRole", "name": "cluster-admin"},
			"subjects": [{"kind": "Group", "name": "system:authenticated"}]}`)}, nil, nil,
			"the RoleBinding argocd/admin (ClusterRole cluster-admin) gives the ApplicationSet controller * on * (*) in the namespace argocd"},
		{"application namespace that is not permitted", []kube.RoleBinding{binding(t, `{"metadata": {"name": "appsets", "namespace": "team-b"}, "roleRef": {"kind": "ClusterRole", "name": "placements"},
			"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "argocd"}]}`)}, nil, []string{"team-a"},
			"in the namespace team-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkAppSetControllerRBAC(appSetSA, tt.appNamespaces, chartRBACWith(t, tt.rbs, tt.crbs))
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

	// A service account of another namespace with the same name.
	other := binding(t, `{"metadata": {"name": "x"}, "roleRef": {"kind": "ClusterRole", "name": "cluster-admin"},
		"subjects": [{"kind": "ServiceAccount", "name": "argocd-applicationset-controller", "namespace": "other"}]}`)
	if p := checkAppSetControllerRBAC(appSetSA, nil, chartRBACWith(t, nil, []kube.RoleBinding{other})); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}
