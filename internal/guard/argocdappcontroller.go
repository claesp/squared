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
	kinds := gitops.TenantProjectKinds()
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

// projectRBAC are the rights of a component of Argo CD that changes the
// objects of the applications.
type projectRBAC struct {
	who        string     // For the messages.
	ownRules   []rbacRule // The Role of the Helm chart in the namespace of Argo CD.
	writeVerbs []string   // The verbs on the kinds that the projects permit.
	bindRoles  []string   // The ClusterRoles that it may bind, by name.
}

// check returns the problems of the rights of the service account (sa) of
// the component. It may have:
//   - in the namespace of Argo CD: the Role of the Helm chart;
//   - in the cluster: the rights that Kubernetes gives to each user, and
//     get on URLs that are not resources;
//   - in each namespace and in the cluster: writeVerbs on the kinds that
//     the projects permit (kinds), and read rights on all kinds but
//     Secrets, for the health and the resource tree;
//   - the verb bind on the ClusterRoles in bindRoles, by name.
//
// Read rights on Secrets everywhere, the verbs escalate and impersonate,
// and bind on other roles are more: with them, the component, and each
// person who can drive it, can get more rights.
func (c projectRBAC) check(sa rbacSubject, kinds []gitops.GroupKind, rbac *kube.RBAC) []string {
	permit := func(namespace, group, resource, verb string, names []string) bool {
		secrets := (group == "" || group == "*") && (resource == "secrets" || resource == "*")
		switch {
		case namespace == sa.Namespace && rulesAllow(c.ownRules, group, resource, verb):
			return true
		case namespace == "" && rulesAllow(defaultClusterRules, group, resource, verb):
			return true
		case slices.Contains(readVerbs, verb) && !secrets:
			return true
		case slices.Contains(c.writeVerbs, verb) && kindPermitted(kinds, group, resource):
			return true
		case verb == "bind" && group == "rbac.authorization.k8s.io" && resource == "clusterroles":
			return len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return !slices.Contains(c.bindRoles, n) })
		}
		return false
	}
	return checkRBAC(c.who, sa, rbac, permit, "the projects permit")
}

// checkAppControllerRBAC returns the problems of the rights of the service
// account (sa) of the application controller. The Helm chart gives it all
// rights in the cluster, because Argo CD can deploy each kind. But the
// projects tell which kinds Argo CD deploys. It may bind the ClusterRoles
// in bindRoles: the platform repository binds the ClusterRole of the
// worker in each tenant namespace.
func checkAppControllerRBAC(sa rbacSubject, kinds []gitops.GroupKind, bindRoles []string, rbac *kube.RBAC) []string {
	return projectRBAC{"the application controller", appControllerRules, writeVerbs, bindRoles}.check(sa, kinds, rbac)
}

// argoServer is the workload of the API server of Argo CD.
const argoServer = "argocd-server"

// serverRules are the rights of the API server in the namespace of Argo
// CD: the Role of the Argo CD Helm chart 10.
var serverRules = []rbacRule{
	{[]string{""}, []string{"secrets", "configmaps"}, []string{"create", "get", "list", "watch", "update", "patch", "delete"}},
	{[]string{"argoproj.io"}, []string{"applications", "applicationsets", "appprojects"},
		[]string{"create", "get", "list", "watch", "update", "delete", "patch"}},
	{[]string{""}, []string{"events"}, []string{"create", "list"}},
}

// serverWriteVerbs are the verbs that the API server needs for the objects
// of the applications: patch for the resource actions, for example a
// restart, and delete for the removal of an object in the web interface.
var serverWriteVerbs = []string{"patch", "delete"}

// checkServerRBAC returns the problems of the rights of the service
// account (sa) of the API server. The Helm chart gives it get, patch and
// delete on all kinds in the cluster, also on Secrets and on
// ClusterRoleBindings, and create on Jobs and Workflows. The server needs
// read rights to show the objects and the logs of the applications, and
// patch and delete only on the kinds that the projects permit. Its own
// RBAC permits each action for each user, but an error in the server or a
// stolen session gets its Kubernetes rights.
func checkServerRBAC(sa rbacSubject, kinds []gitops.GroupKind, rbac *kube.RBAC) []string {
	return projectRBAC{"the API server", serverRules, serverWriteVerbs, nil}.check(sa, kinds, rbac)
}

// argoRepoServer is the workload of the repository server of Argo CD.
const argoRepoServer = "argocd-repo-server"

// checkRepoServer returns the problems of the rights and of the token of
// the repository server (sa, token). The repository server runs Git,
// Helm, Kustomize and plugins with the content of the repositories, thus a
// bad repository or an error in a tool runs code with its rights. It gets
// all that it needs from the other components of Argo CD, and needs no
// rights and no token of the Kubernetes API.
func checkRepoServer(sa rbacSubject, token *kube.WorkloadToken, rbac *kube.RBAC) []string {
	return checkNoAPI("the repository server", "a tool or a plugin that runs with the content of a repository can use it", sa, token, rbac)
}

// argoRedis is the workload of the Redis of Argo CD.
const argoRedis = "argocd-redis"

// checkRedis returns the problems of the rights and of the token of the
// Redis of Argo CD (sa, token). Redis is the cache of Argo CD: the
// manifests, the resource trees and the state of the applications. It
// needs no rights and no token of the Kubernetes API.
func checkRedis(sa rbacSubject, token *kube.WorkloadToken, rbac *kube.RBAC) []string {
	return checkNoAPI("Redis", "an error in Redis gives it to each client that can connect", sa, token, rbac)
}

// checkNoAPI returns the problems of a component (who) that needs no
// rights and no token of the Kubernetes API. It may have only the rights
// that Kubernetes gives to each user. Its pods must not get a token: else
// each right that is given later to a group of all service accounts also
// reaches it. risk tells who can use the token.
func checkNoAPI(who, risk string, sa rbacSubject, token *kube.WorkloadToken, rbac *kube.RBAC) []string {
	permit := func(namespace, group, resource, verb string, _ []string) bool {
		return namespace == "" && rulesAllow(defaultClusterRules, group, resource, verb)
	}
	p := checkRBAC(who, sa, rbac, permit, "it needs")
	if token.Mounted != "" {
		p = append(p, fmt.Sprintf("its pods get a token of the Kubernetes API (%s), which it does not need: %s", token.Mounted, risk))
	}
	if sa.Name == "default" {
		p = append(p, "it has the service account default of the namespace, which other pods can also have, thus each right of that account reaches it")
	}
	return p
}

// redisSecretInit is the service account of the job of the Helm chart that
// makes the password of Redis.
const redisSecretInit = "argocd-redis-secret-init"

// checkRedisSecretInit returns the problems and the warnings of the
// service account of the job that makes the password of Redis (sa). The
// Role of the Helm chart lets it make Secrets in the namespace of Argo
// CD: Kubernetes cannot limit create to one name. With this right, a pod
// with the account can add a cluster Secret or a repository credential,
// which Argo CD reads by their labels. The service account stays after
// the job. The installation can make the Secret argocd-redis itself, and
// turn the job off.
func checkRedisSecretInit(sa rbacSubject, rbac *kube.RBAC) (problems, warnings []string) {
	permit := func(namespace, group, resource, verb string, names []string) bool {
		switch {
		case namespace == "":
			return rulesAllow(defaultClusterRules, group, resource, verb)
		case namespace != sa.Namespace || group != "" || resource != "secrets":
			return false
		case verb == "create":
			return true
		case verb == "get":
			return len(names) == 1 && names[0] == argoRedis
		}
		return false
	}
	problems = checkRBAC("the service account "+sa.Name, sa, rbac, permit, "the job needs")
	bound := slices.ContainsFunc(rbac.RoleBindings, func(b kube.RoleBinding) bool {
		return slices.ContainsFunc(b.Subjects, func(s kube.Subject) bool {
			return s.Kind == "ServiceAccount" && s.Name == sa.Name && s.Namespace == sa.Namespace
		})
	})
	if bound {
		warnings = append(warnings, fmt.Sprintf("the service account %s of the Helm chart can make Secrets in the namespace %s, also cluster Secrets and repository credentials, which Argo CD reads. Set redisSecretInit.enabled to false, and make the Secret %s with the key auth yourself",
			sa.Name, sa.Namespace, argoRedis))
	}
	return problems, warnings
}

// argoDex is the workload and the service account of Dex, the identity
// broker of Argo CD.
const argoDex = "argocd-dex-server"

// dexRules are the rights of Dex in the namespace of Argo CD: the Role of
// the Argo CD Helm chart 10. Dex reads its configuration in argocd-cm and
// the secrets of its connectors in the Secrets.
var dexRules = []rbacRule{
	{[]string{""}, []string{"secrets", "configmaps"}, []string{"get", "list", "watch"}},
}

// checkDex returns the problems and the warnings of Dex (sa). Dex answers
// the sign-ins from the browser, and its Role lets it read all Secrets of
// the namespace of Argo CD: argocd-secret with the key that signs the
// sessions of Argo CD, the repository credentials and the cluster Secrets.
// Squared signs in to Argo CD with Keycloak directly (oidc.config), thus
// Dex has no work when argocd-cm has no dex.config (dexConfig). running
// tells if the Deployment of Dex exists. A binding for its service
// account with more rights than the Role of the chart is a problem, also
// without the Deployment.
func checkDex(sa rbacSubject, running, dexConfig bool, rbac *kube.RBAC) (problems, warnings []string) {
	permit := func(namespace, group, resource, verb string, _ []string) bool {
		return namespace == sa.Namespace && rulesAllow(dexRules, group, resource, verb) ||
			namespace == "" && rulesAllow(defaultClusterRules, group, resource, verb)
	}
	problems = checkRBAC("Dex", sa, rbac, permit, "Dex needs")
	if running && !dexConfig {
		warnings = append(warnings, fmt.Sprintf("Dex runs, but argocd-cm has no dex.config, thus Argo CD does not use it. Dex answers sign-ins and can read all Secrets of the namespace %s. Set dex.enabled to false", sa.Namespace))
	}
	return problems, warnings
}
