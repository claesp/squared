package guard

import (
	"fmt"
	"slices"
	"strings"

	"squared/internal/kube"
)

// rbacRule is a set of rights: each verb on each resource of each group.
type rbacRule struct {
	Groups, Resources, Verbs []string
}

func (r rbacRule) allows(group, resource, verb string) bool {
	return slices.Contains(r.Groups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb)
}

// appSetControllerRules are the rights of the ApplicationSet controller in
// the namespace of Argo CD: the Role of the Argo CD Helm chart 10.
var appSetControllerRules = []rbacRule{
	{[]string{"argoproj.io"}, []string{"applications", "applicationsets", "applicationsets/finalizers"},
		[]string{"create", "delete", "get", "list", "patch", "update", "watch"}},
	{[]string{"argoproj.io"}, []string{"applicationsets/status"}, []string{"get", "patch", "update"}},
	{[]string{"argoproj.io"}, []string{"appprojects"}, []string{"get", "list", "watch"}},
	{[]string{""}, []string{"events"}, []string{"create", "get", "list", "patch", "watch"}},
	{[]string{""}, []string{"configmaps"}, []string{"create", "update", "delete", "get", "list", "patch", "watch"}},
	{[]string{""}, []string{"secrets"}, []string{"get", "list", "watch"}},
	{[]string{"apps", "extensions"}, []string{"deployments"}, []string{"get", "list", "watch"}},
	{[]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create", "get", "update"}},
}

// appSetNamespaceRules are the rights of the ApplicationSet controller for
// ApplicationSets in other namespaces (application namespaces).
var appSetNamespaceRules = []rbacRule{
	appSetControllerRules[0], appSetControllerRules[1], appSetControllerRules[2], appSetControllerRules[3],
}

// defaultClusterRules are the rights in the cluster that Kubernetes gives
// to each authenticated user (system:basic-user and
// system:cluster-trust-bundle-discovery).
var defaultClusterRules = []rbacRule{
	{[]string{"authorization.k8s.io"}, []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}, []string{"create"}},
	{[]string{"authentication.k8s.io"}, []string{"selfsubjectreviews"}, []string{"create"}},
	{[]string{"certificates.k8s.io"}, []string{"clustertrustbundles"}, []string{"get", "list", "watch"}},
}

var readVerbs = []string{"get", "list", "watch"}

// rbacSubject is the service account of a component.
type rbacSubject struct {
	Namespace, Name string
}

// matches tells if a subject of a binding is the service account: by name,
// as user, or as one of the groups that Kubernetes gives it.
func (s rbacSubject) matches(kind, name, namespace string) bool {
	switch kind {
	case "ServiceAccount":
		return name == s.Name && namespace == s.Namespace
	case "User":
		return name == "system:serviceaccount:"+s.Namespace+":"+s.Name
	case "Group":
		return name == "system:authenticated" || name == "system:serviceaccounts" || name == "system:serviceaccounts:"+s.Namespace
	}
	return false
}

// checkAppSetControllerRBAC returns the problems of the rights of the
// service account (sa) of the ApplicationSet controller. Argo CD runs it
// in its namespace (own). The controller reads all Secrets there, and
// each ApplicationSet drives it: more rights give each person who can
// make an ApplicationSet more power, for example through the kind of a
// cluster decision resource. The controller may have:
//   - in the namespace of Argo CD: the Role of the Helm chart, and read
//     rights for other kinds, for example of cluster decision resources;
//   - in the application namespaces (appNamespaces), and in the cluster
//     when there are application namespaces: the rights for the
//     applications and the ApplicationSets;
//   - in the cluster: the rights that Kubernetes gives to each user, and
//     get on URLs that are not resources.
func checkAppSetControllerRBAC(sa rbacSubject, appNamespaces []string, rbac *kube.RBAC) []string {
	role := func(kind, namespace, name string) *kube.Role {
		list := rbac.ClusterRoles
		if kind == "Role" {
			list = rbac.Roles
		}
		for i := range list {
			if list[i].Metadata.Name == name && (kind != "Role" || list[i].Metadata.Namespace == namespace) {
				return &list[i]
			}
		}
		return nil
	}
	// allowed returns the rules for a namespace ("" for the cluster), and
	// if the controller may read each kind there.
	allowed := func(namespace string) ([]rbacRule, bool) {
		switch {
		case namespace == sa.Namespace:
			return appSetControllerRules, true
		case namespace == "" && len(appNamespaces) > 0:
			return append(slices.Clone(defaultClusterRules), appSetNamespaceRules...), false
		case namespace == "":
			return defaultClusterRules, false
		case slices.Contains(appNamespaces, namespace):
			return appSetNamespaceRules, false
		}
		return nil, false
	}

	var p []string
	check := func(bindingKind string, b kube.RoleBinding, namespace string) {
		if !slices.ContainsFunc(b.Subjects, func(s kube.Subject) bool { return sa.matches(s.Kind, s.Name, s.Namespace) }) {
			return
		}
		r := role(b.RoleRef.Kind, b.Metadata.Namespace, b.RoleRef.Name)
		if r == nil {
			return
		}
		rules, readAll := allowed(namespace)
		scope := "the cluster"
		if namespace != "" {
			scope = "the namespace " + namespace
		}
		where := fmt.Sprintf("the %s %s (%s %s)", bindingKind, b.Metadata.Name, b.RoleRef.Kind, b.RoleRef.Name)
		if namespace != "" {
			where = fmt.Sprintf("the %s %s/%s (%s %s)", bindingKind, namespace, b.Metadata.Name, b.RoleRef.Kind, b.RoleRef.Name)
		}
		for _, rule := range r.Rules {
			if len(rule.NonResourceURLs) > 0 {
				// Only a ClusterRoleBinding gives them, for the cluster.
				if slices.ContainsFunc(rule.Verbs, func(v string) bool { return v != "get" }) {
					p = append(p, fmt.Sprintf("%s gives the ApplicationSet controller %s on the URLs %s, more than Argo CD needs",
						where, strings.Join(rule.Verbs, ", "), strings.Join(rule.NonResourceURLs, ", ")))
				}
				continue
			}
			var extra []string
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					var verbs []string
					for _, v := range rule.Verbs {
						ok := slices.ContainsFunc(rules, func(a rbacRule) bool { return a.allows(g, res, v) }) ||
							readAll && slices.Contains(readVerbs, v)
						if !ok {
							verbs = append(verbs, v)
						}
					}
					if len(verbs) > 0 {
						extra = append(extra, fmt.Sprintf("%s on %s (%s)", strings.Join(verbs, " "), res, cmpOrCore(g)))
					}
				}
			}
			if len(extra) > 0 {
				p = append(p, fmt.Sprintf("%s gives the ApplicationSet controller %s in %s, more than Argo CD needs",
					where, strings.Join(extra, ", "), scope))
			}
		}
	}
	for _, b := range rbac.ClusterRoleBindings {
		check("ClusterRoleBinding", b, "")
	}
	for _, b := range rbac.RoleBindings {
		check("RoleBinding", b, b.Metadata.Namespace)
	}
	return p
}

// cmpOrCore returns the name of an API group for a message.
func cmpOrCore(group string) string {
	if group == "" {
		return "core"
	}
	return group
}
