package guard

import (
	"fmt"
	"slices"
	"strings"

	"squared/internal/keycloak"
)

// keycloakMapperTypes are the OIDC protocol mapper types of Keycloak 26.8
// (org.keycloak.protocol.oidc.mappers and the organization mappers). A
// mapper of another type, for example a script mapper or a mapper of an
// extension, can write any claim.
var keycloakMapperTypes = []string{
	"oidc-acr-mapper", "oidc-address-mapper", "oidc-allowed-origins-mapper", "oidc-amr-mapper",
	"oidc-audience-mapper", "oidc-audience-resolve-mapper", "oidc-claims-param-token-mapper",
	"oidc-claims-param-value-idtoken-mapper", "oidc-full-name-mapper", "oidc-group-membership-mapper",
	"oidc-hardcoded-claim-mapper", "oidc-hardcoded-role-mapper", "oidc-nonce-backwards-compatible-mapper",
	"oidc-organization-group-membership-mapper", "oidc-organization-membership-mapper",
	"oidc-parameterized-scope-audience-mapper", "oidc-parameterized-scope-client-sub-mapper",
	"oidc-parameterized-scope-mapper", "oidc-parameterized-scope-user-property-mapper",
	"oidc-role-name-mapper", "oidc-session-state-mapper", "oidc-sha256-pairwise-sub-mapper",
	"oidc-sub-mapper", "oidc-usermodel-attribute-mapper", "oidc-usermodel-client-role-mapper",
	"oidc-usermodel-property-mapper", "oidc-usermodel-realm-role-mapper", "oidc-usersessionmodel-note-mapper",
}

const keycloakGroupMapper = "oidc-group-membership-mapper"

// mapperRules are the inputs of checkKeycloakMappers.
type mapperRules struct {
	// Clients are the client IDs whose tokens the services accept.
	Clients []string
	// Audiences are custom audiences that a service accepts, for example
	// "squared-api". A client with an audience mapper for one of these
	// audiences or for one of Clients is checked too.
	Audiences []string
	// Claims are the claims that the services read as group paths.
	Claims []string
	// Ignore are unknown mapper types that an operator checked.
	Ignore []string
}

// claimRule tells which mappers may write a claim that gives rights.
type claimRule struct {
	claim string
	what  string // "group", "email" or "email verification", for messages.
	// check returns "" if the mapper may write the claim, else the reason.
	check func(m keycloak.ProtocolMapper) string
}

// claimRules returns the rules for the group claims and for the email
// claims. Squared and Forgejo link a new sign-in to an existing account
// with the same email address, and Squared does this only if
// email_verified is true. Thus these claims must come from the user: a
// fixed value, another attribute (which the user can perhaps change), or
// a value from the sign-in request gives the account of another person.
func claimRules(groupClaims []string) []claimRule {
	var rules []claimRule
	for _, c := range groupClaims {
		rules = append(rules, claimRule{claim: c, what: "group", check: func(m keycloak.ProtocolMapper) string {
			switch {
			case m.ProtocolMapper != keycloakGroupMapper:
				return "only " + keycloakGroupMapper + " may write it"
			case m.Config["full.path"] != "true":
				return "it writes short group names; a subgroup with the same name then gives the same rights"
			}
			return ""
		}})
	}
	userField := func(attribute string) func(m keycloak.ProtocolMapper) string {
		return func(m keycloak.ProtocolMapper) string {
			if (m.ProtocolMapper == "oidc-usermodel-property-mapper" || m.ProtocolMapper == "oidc-usermodel-attribute-mapper") &&
				m.Config["user.attribute"] == attribute {
				return ""
			}
			return "only a user property or user attribute mapper for " + attribute + " may write it"
		}
	}
	return append(rules,
		claimRule{claim: "email", what: "email", check: userField("email")},
		claimRule{claim: "email_verified", what: "email verification", check: userField("emailVerified")})
}

// checkKeycloakMappers checks the mappers that write the group and email
// claims of the tokens that the services accept (see claimRules). Other
// mappers can put any value into such a claim, for example a fixed group,
// a user attribute, or a value from the sign-in request. Then a person has
// the rights of a group without membership, or the account of another
// person, and the protection of the group does not help. The function
// returns warnings and an error that names each problem.
func checkKeycloakMappers(clients []keycloak.ClientMappers, scopes []keycloak.ClientScope, r mapperRules) ([]string, error) {
	byName := map[string]keycloak.ClientScope{}
	for _, s := range scopes {
		byName[s.Name] = s
	}
	rules := claimRules(r.Claims)
	var warnings, problems []string
	for _, c := range clients {
		why := ""
		if slices.Contains(r.Clients, c.ClientID) {
			why = "a service uses it"
		} else if a := audienceFor(c, byName, r); a != "" {
			why = "its tokens have the audience " + a
		} else {
			continue
		}
		allowed := map[string]bool{}
		for _, m := range effectiveMappers(c, byName) {
			if m.mapper.Protocol != "" && m.mapper.Protocol != "openid-connect" {
				continue
			}
			where := fmt.Sprintf("client %s (%s) %s mapper %q", c.ClientID, why, m.place, m.mapper.Name)
			typ := m.mapper.ProtocolMapper
			if !slices.Contains(keycloakMapperTypes, typ) {
				if !slices.Contains(r.Ignore, typ) {
					problems = append(problems, fmt.Sprintf("%s has the unknown type %s, which can write any claim", where, typ))
				}
				continue
			}
			name := m.mapper.Config["claim.name"]
			for _, rule := range rules {
				if name != rule.claim && !strings.HasPrefix(name, rule.claim+".") {
					continue // "groups.x" makes the claim "groups" an object.
				}
				if reason := rule.check(m.mapper); reason != "" {
					problems = append(problems, fmt.Sprintf("%s (type %s) writes the %s claim %s: %s", where, typ, rule.what, name, reason))
				} else {
					allowed[rule.claim] = true
				}
			}
		}
		if slices.Contains(r.Clients, c.ClientID) {
			for _, rule := range rules {
				if !allowed[rule.claim] {
					warnings = append(warnings, fmt.Sprintf("Keycloak client %s has no mapper for the %s claim %s", c.ClientID, rule.what, rule.claim))
				}
			}
		}
	}
	// Without the role view-clients, Keycloak shows no clients. Thus a
	// missing client is a problem: the guard must not check nothing.
	for _, id := range r.Clients {
		if !slices.ContainsFunc(clients, func(c keycloak.ClientMappers) bool { return c.ClientID == id }) {
			problems = append(problems, fmt.Sprintf("the guard does not see the client %s (it needs the role view-clients)", id))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("mappers of Keycloak can put values into claims that give rights: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

type placedMapper struct {
	place  string // "own" or "scope <name>"
	mapper keycloak.ProtocolMapper
}

// effectiveMappers returns the mappers of a client and of its default and
// optional client scopes. A client can ask for an optional scope at each
// sign-in, thus its mappers count too.
func effectiveMappers(c keycloak.ClientMappers, scopes map[string]keycloak.ClientScope) []placedMapper {
	var out []placedMapper
	for _, m := range c.ProtocolMappers {
		out = append(out, placedMapper{place: "own", mapper: m})
	}
	for _, name := range slices.Concat(c.DefaultClientScopes, c.OptionalClientScopes) {
		for _, m := range scopes[name].ProtocolMappers {
			out = append(out, placedMapper{place: "scope " + name, mapper: m})
		}
	}
	return out
}

// audienceFor returns the audience of rules that a mapper of the client
// adds to its tokens, or "".
func audienceFor(c keycloak.ClientMappers, scopes map[string]keycloak.ClientScope, r mapperRules) string {
	for _, m := range effectiveMappers(c, scopes) {
		if m.mapper.ProtocolMapper != "oidc-audience-mapper" {
			continue
		}
		if a := m.mapper.Config["included.client.audience"]; a != "" && slices.Contains(r.Clients, a) {
			return a
		}
		if a := m.mapper.Config["included.custom.audience"]; a != "" && slices.Contains(r.Audiences, a) {
			return a
		}
	}
	return ""
}
