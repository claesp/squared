package guard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"squared/internal/gitops"
	"squared/internal/kube"
)

// argoAppController is the workload of the application controller.
const argoAppController = "argocd-application-controller"

// appControllerRules are the rights of the application controller in the
// namespace of Argo CD: the Role of the Argo CD Helm chart 10.
var appControllerRules = []rbacRule{
	{[]string{""}, []string{"secrets", "configmaps"}, []string{"get", "list", "watch"}},
	{[]string{"argoproj.io"}, []string{"applications", "applicationsets", "appprojects"},
		[]string{"create", "get", "list", "watch", "update", "patch", "delete"}},
	{[]string{""}, []string{"events"}, []string{"create", "list"}},
	{[]string{"apps"}, []string{"deployments"}, []string{"get", "list", "watch"}},
}

// writeVerbs are the verbs that the application controller needs for a
// kind that it deploys.
var writeVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}

// projectKinds returns the kinds that the AppProjects permit: the cluster
// kinds and the namespaced kinds. A project without a source repository or
// without a destination permits nothing. A project without a list of
// namespaced kinds permits all of them. The kinds of the tenant projects
// are always in the list: Squared makes a tenant project for each new
// tenant, also when there is no tenant yet.
func projectKinds(projects []kube.AppProject) ([]gitops.GroupKind, error) {
	kinds := slices.Clone(gitops.TenantKinds)
	for _, p := range projects {
		var obj struct {
			Spec struct {
				SourceRepos                []string            `json:"sourceRepos"`
				Destinations               []json.RawMessage   `json:"destinations"`
				ClusterResourceWhitelist   []gitops.GroupKind  `json:"clusterResourceWhitelist"`
				NamespaceResourceWhitelist *[]gitops.GroupKind `json:"namespaceResourceWhitelist"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(p.Raw, &obj); err != nil {
			return nil, fmt.Errorf("project %s: %w", p.Metadata.Name, err)
		}
		s := obj.Spec
		if len(s.SourceRepos) == 0 || len(s.Destinations) == 0 {
			continue
		}
		kinds = append(kinds, s.ClusterResourceWhitelist...)
		if s.NamespaceResourceWhitelist == nil {
			kinds = append(kinds, gitops.GroupKind{Group: "*", Kind: "*"})
		} else {
			kinds = append(kinds, *s.NamespaceResourceWhitelist...)
		}
	}
	return kinds, nil
}

// kindResource returns the resource of a kind, as the API server names
// it: for example "networkpolicies" for NetworkPolicy and "ingresses" for
// Ingress.
func kindResource(kind string) string {
	r := strings.ToLower(kind)
	switch {
	case r == "endpoints":
		// The kind has the plural name.
		return r
	case len(r) > 1 && strings.HasSuffix(r, "y") && !strings.ContainsAny(r[len(r)-2:len(r)-1], "aeiou"):
		return r[:len(r)-1] + "ies"
	case strings.HasSuffix(r, "s") || strings.HasSuffix(r, "x") || strings.HasSuffix(r, "ch") || strings.HasSuffix(r, "sh"):
		return r + "es"
	}
	return r + "s"
}

// kindPermitted tells if the kinds permit a resource of an API group. A
// "*" in the rule needs a "*" in the kinds.
func kindPermitted(kinds []gitops.GroupKind, group, resource string) bool {
	return slices.ContainsFunc(kinds, func(k gitops.GroupKind) bool {
		groupOK := k.Group == "*" || group != "*" && k.Group == group
		resourceOK := k.Kind == "*" || resource != "*" && kindResource(k.Kind) == resource
		return groupOK && resourceOK
	})
}

// checkAppControllerRBAC returns the problems of the rights of the service
// account (sa) of the application controller. The Helm chart gives it all
// rights in the cluster, because Argo CD can deploy each kind. But the
// projects tell which kinds Argo CD deploys. The controller may have:
//   - in the namespace of Argo CD: the Role of the Helm chart;
//   - in the cluster: the rights that Kubernetes gives to each user, and
//     get on URLs that are not resources;
//   - in each namespace and in the cluster: the write rights on the kinds
//     that the projects permit (kinds), and read rights on all kinds but
//     Secrets, for the health and the resource tree;
//   - the verb bind on the ClusterRoles in bindRoles, by name: the
//     platform repository binds the ClusterRole of the worker in each
//     tenant namespace.
//
// Read rights on Secrets everywhere, the verbs escalate and impersonate,
// and bind on other roles are more: with them, the controller, and each
// person who can change what it deploys, can get more rights.
func checkAppControllerRBAC(sa rbacSubject, kinds []gitops.GroupKind, bindRoles []string, rbac *kube.RBAC) []string {
	permit := func(namespace, group, resource, verb string, names []string) bool {
		secrets := (group == "" || group == "*") && (resource == "secrets" || resource == "*")
		switch {
		case namespace == sa.Namespace && rulesAllow(appControllerRules, group, resource, verb):
			return true
		case namespace == "" && rulesAllow(defaultClusterRules, group, resource, verb):
			return true
		case slices.Contains(readVerbs, verb) && !secrets:
			return true
		case slices.Contains(writeVerbs, verb) && kindPermitted(kinds, group, resource):
			return true
		case verb == "bind" && group == "rbac.authorization.k8s.io" && resource == "clusterroles":
			return len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return !slices.Contains(bindRoles, n) })
		}
		return false
	}
	return checkRBAC("the application controller", sa, rbac, permit, "the projects permit")
}
