package guard

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// argoRBACKeys are the keys of the ConfigMap argocd-rbac-cm that Argo CD
// 3.5 reads (util/rbac/rbac.go). Argo CD also reads all keys
// policy.<name>.csv.
var argoRBACKeys = []string{"policy.csv", "policy.default", "policy.matchMode", "scopes"}

var argoPolicyKey = regexp.MustCompile(`^policy(\..+)?\.csv$`)

// argoScopeClaims are the claims that the guard knows in the key scopes.
// Argo CD compares the values of these claims with the subjects of the
// "g" lines. The groups claim has the Keycloak group paths. An email
// address is not a group.
var argoScopeClaims = []string{"groups", "email"}

// checkArgoRBAC returns the groups that the ConfigMap argocd-rbac-cm gives
// rights to. It returns an error if the ConfigMap has a key or a claim
// that the guard does not know, because the guard can then miss a group.
// ignore are unknown keys that an operator checked and accepted.
func checkArgoRBAC(data map[string]string, ignore []string) ([]Reference, []string, error) {
	var warnings, problems []string
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if slices.Contains(argoRBACKeys, k) || argoPolicyKey.MatchString(k) || slices.Contains(ignore, k) {
			continue
		}
		// All keys of this ConfigMap are about rights.
		problems = append(problems, "new key "+k)
	}

	// Without the key, Argo CD uses the groups claim.
	scopes := []string{"groups"}
	if v := strings.TrimSpace(data["scopes"]); v != "" {
		var err error
		if scopes, err = parseScopes(v); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, claim := range scopes {
		if !slices.Contains(argoScopeClaims, claim) {
			problems = append(problems, fmt.Sprintf("scopes has the claim %s; the guard cannot tell which groups give a value of it", claim))
		}
	}
	if !slices.Contains(scopes, "groups") {
		warnings = append(warnings, "Argo CD scopes has no groups claim: the group lines give no rights now")
	}

	// Argo CD adds the keys policy.<name>.csv after policy.csv.
	var refs []Reference
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if !argoPolicyKey.MatchString(k) {
			continue
		}
		refs = append(refs, argoLineRefs(data[k], k)...)
	}
	if len(problems) > 0 {
		return refs, warnings, fmt.Errorf("the RBAC configuration of Argo CD changed: %s. The guard keeps the current protection. Update the guard for this Argo CD version or change the configuration",
			strings.Join(problems, "; "))
	}
	return refs, warnings, nil
}

// argoLineRefs returns the subjects of the "g" lines of a policy. A short
// name never matches the full paths of the groups claim. It is returned
// too, because the guard reports it as a configuration error. An email
// address is a user, not a group.
func argoLineRefs(csv, where string) []Reference {
	groups, others := argoRBACGroups(csv)
	var refs []Reference
	for _, g := range groups {
		refs = append(refs, Reference{Group: g, Where: where})
	}
	for _, o := range others {
		if !strings.Contains(o, "@") {
			refs = append(refs, Reference{Group: o, Where: where})
		}
	}
	return refs
}

// parseScopes reads the key scopes, which is a YAML list of claim names,
// for example "[groups, email]" or a block list with "- groups" lines.
func parseScopes(v string) ([]string, error) {
	var items []string
	if inner, ok := strings.CutPrefix(v, "["); ok {
		inner, ok = strings.CutSuffix(inner, "]")
		if !ok {
			return nil, fmt.Errorf("scopes %q is not a list", v)
		}
		items = strings.Split(inner, ",")
	} else {
		for _, line := range strings.Split(v, "\n") {
			item, ok := strings.CutPrefix(strings.TrimSpace(line), "- ")
			if !ok && strings.TrimSpace(line) != "" {
				return nil, fmt.Errorf("scopes %q is not a list", v)
			}
			if ok {
				items = append(items, item)
			}
		}
	}
	var claims []string
	for _, item := range items {
		item = strings.Trim(strings.TrimSpace(item), `"'`)
		if item != "" {
			claims = append(claims, item)
		}
	}
	return claims, nil
}

// argoProjectSpecFields are the fields of an AppProject spec in Argo CD
// 3.5, and argoProjectRoleFields are the fields of a project role.
// Kubernetes removes fields that the CRD does not have. Thus a new field
// tells that an upgrade of Argo CD added it.
var (
	argoProjectSpecFields = []string{
		"clusterResourceBlacklist", "clusterResourceWhitelist", "description", "destinationServiceAccounts",
		"destinations", "namespaceResourceBlacklist", "namespaceResourceWhitelist", "orphanedResources",
		"permitOnlyProjectScopedClusters", "roles", "signatureKeys", "sourceIntegrity", "sourceNamespaces",
		"sourceRepos", "syncWindows",
	}
	argoProjectRoleFields = []string{"description", "groups", "jwtTokens", "name", "policies"}
)

// checkArgoProjectFields returns the warnings and the problems for the
// field names of an AppProject. ignore are new fields that an operator
// checked, as "spec.<field>" or "roles.<field>".
func checkArgoProjectFields(raw json.RawMessage, ignore []string) (warnings, problems []string, err error) {
	var obj struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, err
	}
	check := func(prefix, field string) {
		name := prefix + field
		if slices.Contains(ignore, name) {
			return
		}
		if rightsLike.MatchString(field) {
			problems = append(problems, "new field "+name+" can give rights")
		} else {
			warnings = append(warnings, fmt.Sprintf("the new field %s; check that it gives no rights to groups, then add it to ignoreFields", name))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(obj.Spec)) {
		if !slices.Contains(argoProjectSpecFields, k) {
			check("spec.", k)
		}
	}
	var roles []map[string]json.RawMessage
	if r, ok := obj.Spec["roles"]; ok {
		if err := json.Unmarshal(r, &roles); err != nil {
			return nil, nil, err
		}
	}
	seen := map[string]bool{}
	for _, role := range roles {
		for _, k := range slices.Sorted(maps.Keys(role)) {
			if !slices.Contains(argoProjectRoleFields, k) && !seen[k] {
				seen[k] = true
				check("roles.", k)
			}
		}
	}
	return warnings, problems, nil
}
