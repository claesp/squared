package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"squared/internal/gitops"
)

// argoProjectSpec is the part of an AppProject spec that limits what its
// applications can do.
type argoProjectSpec struct {
	SourceRepos  []string `json:"sourceRepos"`
	Destinations []struct {
		Server    string `json:"server"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"destinations"`
	ClusterResourceWhitelist []gitops.GroupKind `json:"clusterResourceWhitelist"`
	// Argo CD permits all namespaced kinds without the list, and none with
	// an empty list. Thus nil and empty are not the same.
	NamespaceResourceWhitelist *[]gitops.GroupKind `json:"namespaceResourceWhitelist"`
	SourceNamespaces           []string            `json:"sourceNamespaces"`
	DestinationServiceAccounts []json.RawMessage   `json:"destinationServiceAccounts"`
	Roles                      []struct {
		Name      string            `json:"name"`
		Policies  []string          `json:"policies"`
		JWTTokens []json.RawMessage `json:"jwtTokens"`
	} `json:"roles"`
}

// tenantProjectRules are what a tenant project may permit.
type tenantProjectRules struct {
	ClusterServers []string // The API servers that Squared deploys to.
	RepoPrefixes   []string // The addresses of the GitOps repositories, for example "https://git.example/squared/".
}

// envName is the rule for the names of environments (domain).
var envName = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// checkTenantProject compares an Argo CD project that Squared made for a
// tenant with what Squared puts into it (gitops.Config.appProject). The
// project is the second line of the isolation of the tenants: Argo CD
// refuses an object outside it, also when a manifest asks for it. The
// project is in the platform repository, and Argo CD changes it back
// after a change by hand, but only while its application heals itself.
func checkTenantProject(slug string, raw json.RawMessage, r tenantProjectRules) ([]string, error) {
	var obj struct {
		Spec argoProjectSpec `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	s := obj.Spec
	var p []string

	for _, d := range s.Destinations {
		switch {
		case d.Name != "" && d.Server == "":
			p = append(p, fmt.Sprintf("a destination names the cluster %q instead of its address", d.Name))
		case !slices.Contains(r.ClusterServers, d.Server):
			p = append(p, fmt.Sprintf("a destination has the cluster %s, to which Squared does not deploy", d.Server))
		}
		env, ok := strings.CutPrefix(d.Namespace, slug+"-")
		if !ok || !envName.MatchString(env) {
			p = append(p, fmt.Sprintf("a destination has the namespace %q, which is not a namespace of the tenant", d.Namespace))
		}
	}

	var want []string
	for _, prefix := range r.RepoPrefixes {
		want = append(want, strings.TrimSuffix(prefix, "/")+"/"+gitops.TenantRepo(slug)+".git")
	}
	for _, repo := range s.SourceRepos {
		if !slices.Contains(want, repo) {
			p = append(p, fmt.Sprintf("the source repository %q is not the repository of the tenant", repo))
		}
	}

	if len(s.ClusterResourceWhitelist) > 0 {
		p = append(p, fmt.Sprintf("it permits cluster objects (%s)", kindList(s.ClusterResourceWhitelist)))
	}
	if s.NamespaceResourceWhitelist == nil {
		p = append(p, "it has no list of namespaced kinds, thus it permits all of them, for example Role and Secret")
	} else {
		var extra []gitops.GroupKind
		for _, k := range *s.NamespaceResourceWhitelist {
			if !slices.Contains(gitops.TenantProjectKinds(), k) {
				extra = append(extra, k)
			}
		}
		if len(extra) > 0 {
			p = append(p, fmt.Sprintf("it permits the kinds %s, which Squared does not make", kindList(extra)))
		}
	}
	if len(s.SourceNamespaces) > 0 {
		p = append(p, fmt.Sprintf("applications in the namespaces %s can use it", strings.Join(s.SourceNamespaces, ", ")))
	}
	if len(s.DestinationServiceAccounts) > 0 {
		p = append(p, "it has destination service accounts, which Squared does not set")
	}

	policies := gitops.MemberPolicies(slug)
	for _, role := range s.Roles {
		switch {
		case role.Name != gitops.MemberRole:
			p = append(p, fmt.Sprintf("it has the role %q, which Squared does not make", role.Name))
		case len(role.JWTTokens) > 0:
			p = append(p, "the role member has tokens, which work without Keycloak")
		}
		if role.Name == gitops.MemberRole {
			for _, pol := range role.Policies {
				if !slices.Contains(policies, strings.Join(strings.Fields(pol), " ")) {
					p = append(p, fmt.Sprintf("the role member has the policy %q, which gives more than read access", pol))
				}
			}
		}
	}
	return p, nil
}

func kindList(kinds []gitops.GroupKind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if k.Group == "" {
			names = append(names, k.Kind)
		} else {
			names = append(names, k.Group+"/"+k.Kind)
		}
	}
	return strings.Join(names, ", ")
}
