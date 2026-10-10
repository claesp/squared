package guard

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// forgejoOAuthType is the type number of OAuth2 authentication sources in
// the Forgejo table login_source.
const forgejoOAuthType = 6

// fieldKind tells what a field of a service configuration means for the
// guard.
type fieldKind int

const (
	fieldRights  fieldKind = iota // Gives rights to groups. The guard reads it.
	fieldRelated                  // Changes the meaning of a rights field. The guard reads it.
	fieldOther                    // Has nothing to do with groups.
)

// forgejoOAuthFields are the fields of the configuration of an OAuth2
// source in Forgejo 15. Forgejo writes all fields, also empty ones. Thus a
// missing field tells that an upgrade renamed or removed it.
var forgejoOAuthFields = map[string]fieldKind{
	"AdminGroup":                    fieldRights,
	"RestrictedGroup":               fieldRights,
	"GroupTeamMap":                  fieldRights,
	"RequiredClaimValue":            fieldRights, // A group, if the required claim is the groups claim.
	"QuotaGroupMap":                 fieldRights, // Gives storage quotas to groups.
	"GroupClaimName":                fieldRelated,
	"RequiredClaimName":             fieldRelated,
	"QuotaGroupClaimName":           fieldRelated,
	"GroupTeamMapRemoval":           fieldRelated,
	"QuotaGroupMapRemoval":          fieldRelated,
	"AllowUsernameChange":           fieldOther,
	"AttributeSSHPublicKey":         fieldOther,
	"ClientID":                      fieldOther,
	"ClientSecret":                  fieldOther,
	"CustomURLMapping":              fieldOther,
	"IconURL":                       fieldOther,
	"OpenIDConnectAutoDiscoveryURL": fieldOther,
	"Provider":                      fieldOther,
	"Scopes":                        fieldOther,
}

// rightsLike matches the names of fields that can give rights. An unknown
// field with such a name stops the source, because the guard does not know
// how to read it.
var rightsLike = regexp.MustCompile(`(?i)group|team|admin|restrict|claim|role|quota|permission|access|org`)

// forgejoSource is one row of the view squared_guard_auth_sources.
type forgejoSource struct {
	ID     int64
	Name   string
	Type   int
	Active bool
	Keys   []string // Names of the keys of cfg. nil if cfg is not a JSON object.

	AdminGroup, RestrictedGroup, GroupTeamMap, GroupClaimName string
	RequiredClaimName, RequiredClaimValue                     string
	QuotaGroupClaimName, QuotaGroupMap                        string
}

// checkForgejoSource returns the groups that an authentication source gives
// rights to. It returns an error if the fields of the source do not agree
// with the fields that the guard knows, because the guard can then miss a
// group. ignore are unknown fields that an operator checked and accepted.
func checkForgejoSource(s forgejoSource, ignore []string) ([]Reference, []string, error) {
	where := fmt.Sprintf("authentication source %s", s.Name)
	if s.Type != forgejoOAuthType {
		// Another type with the fields of an OAuth source tells that
		// Forgejo changed the type numbers.
		if slices.Contains(s.Keys, "Provider") && slices.Contains(s.Keys, "ClientSecret") {
			return nil, nil, fmt.Errorf("%s has type %d but the fields of an OAuth source; the guard expects type %d. Check the Forgejo upgrade and update the guard",
				where, s.Type, forgejoOAuthType)
		}
		return nil, nil, nil // LDAP, SMTP and others do not use the groups of the identity provider.
	}
	if s.Keys == nil {
		return nil, nil, fmt.Errorf("%s: the configuration is not a JSON object", where)
	}

	var warnings []string
	var problems []string
	known := map[string]bool{}
	for _, k := range s.Keys {
		known[k] = true
		if _, ok := forgejoOAuthFields[k]; ok || slices.Contains(ignore, k) {
			continue
		}
		if rightsLike.MatchString(k) {
			problems = append(problems, fmt.Sprintf("new field %s can give rights", k))
		} else {
			warnings = append(warnings, fmt.Sprintf("Forgejo %s has the new field %s; check that it gives no rights to groups, then add it to ignoreFields", where, k))
		}
	}
	names := make([]string, 0, len(forgejoOAuthFields))
	for k := range forgejoOAuthFields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if known[k] {
			continue
		}
		if forgejoOAuthFields[k] == fieldOther {
			warnings = append(warnings, fmt.Sprintf("Forgejo %s has no field %s anymore", where, k))
		} else {
			problems = append(problems, fmt.Sprintf("field %s is missing (renamed or removed)", k))
		}
	}
	if len(problems) > 0 {
		return nil, warnings, fmt.Errorf("the fields of Forgejo %s changed: %s. The guard cannot read the groups of this source; it keeps the current protection. Update the guard for this Forgejo version",
			where, strings.Join(problems, "; "))
	}

	// An inactive source gives no rights now, but somebody can turn it on
	// in the web interface. Thus its groups are protected too.
	var groups []string
	for _, g := range []string{s.AdminGroup, s.RestrictedGroup} {
		if g = strings.TrimSpace(g); g != "" {
			groups = append(groups, g)
		}
	}
	groups = append(groups, teamMapGroups(s.GroupTeamMap)...)
	// A required claim value is a group if the claim is the groups claim,
	// or if the value is a group path. Then only members of that group can
	// sign in.
	if v := strings.TrimSpace(s.RequiredClaimValue); v != "" && s.RequiredClaimName != "" &&
		(s.RequiredClaimName == s.GroupClaimName || strings.HasPrefix(v, "/")) {
		groups = append(groups, v)
	}
	// The keys of the quota map are groups. They get storage quotas.
	groups = append(groups, quotaMapGroups(s.QuotaGroupMap)...)
	refs := make([]Reference, 0, len(groups))
	for _, g := range groups {
		refs = append(refs, Reference{Group: g, Where: where})
	}
	return refs, warnings, nil
}

// quotaMapGroups returns the groups of a Forgejo quota group map, which is
// JSON: {"/group": ["quota-group"]}.
func quotaMapGroups(raw string) []string { return teamMapGroups(raw) }
