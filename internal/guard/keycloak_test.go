package guard

import (
	"context"
	"strings"
	"testing"

	"squared/internal/keycloak"
)

func groupMapper(fullPath string) keycloak.ProtocolMapper {
	return keycloak.ProtocolMapper{Name: "groups", Protocol: "openid-connect", ProtocolMapper: "oidc-group-membership-mapper",
		Config: map[string]string{"claim.name": "groups", "full.path": fullPath}}
}

// realmLikeDevelopment returns clients and scopes as in the development
// realm: the service clients have a group mapper with full paths, and the
// built-in clients may ask for the scope microprofile-jwt, which writes
// realm roles into "groups".
func realmLikeDevelopment() ([]keycloak.ClientMappers, []keycloak.ClientScope) {
	scopes := []keycloak.ClientScope{
		{Name: "email", Protocol: "openid-connect", ProtocolMappers: []keycloak.ProtocolMapper{
			{Name: "email", ProtocolMapper: "oidc-usermodel-attribute-mapper", Config: map[string]string{"claim.name": "email"}}}},
		{Name: "microprofile-jwt", Protocol: "openid-connect", ProtocolMappers: []keycloak.ProtocolMapper{
			{Name: "groups", ProtocolMapper: "oidc-usermodel-realm-role-mapper", Config: map[string]string{"claim.name": "groups"}}}},
		{Name: "role_list", Protocol: "saml", ProtocolMappers: []keycloak.ProtocolMapper{
			{Name: "role list", Protocol: "saml", ProtocolMapper: "saml-role-list-mapper"}}},
	}
	clients := []keycloak.ClientMappers{
		{ClientID: "argocd", DefaultClientScopes: []string{"email", "role_list"}, ProtocolMappers: []keycloak.ProtocolMapper{groupMapper("true")}},
		{ClientID: "squared", DefaultClientScopes: []string{"email"}, ProtocolMappers: []keycloak.ProtocolMapper{groupMapper("true"),
			{Name: "aud", ProtocolMapper: "oidc-audience-mapper", Config: map[string]string{"included.custom.audience": "squared-api"}}}},
		{ClientID: "account", DefaultClientScopes: []string{"email"}, OptionalClientScopes: []string{"microprofile-jwt"}},
	}
	return clients, scopes
}

func rulesForDevelopment() mapperRules {
	return mapperRules{Clients: []string{"argocd", "squared"}, Audiences: []string{"squared-api"}, Claims: []string{"groups"}}
}

func TestCheckKeycloakMappersDevelopment(t *testing.T) {
	clients, scopes := realmLikeDevelopment()
	warnings, err := checkKeycloakMappers(clients, scopes, rulesForDevelopment())
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

func TestCheckKeycloakMappersProblems(t *testing.T) {
	tests := []struct {
		name   string
		change func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope)
		errMsg string
	}{
		{"fixed group", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[0].ProtocolMappers = append(c[0].ProtocolMappers, keycloak.ProtocolMapper{Name: "admins", ProtocolMapper: "oidc-hardcoded-claim-mapper",
				Config: map[string]string{"claim.name": "groups", "claim.value": "/squared-admins"}})
			return c, s
		}, `client argocd (a service uses it) own mapper "admins" (type oidc-hardcoded-claim-mapper) writes the group claim groups`},
		{"short names", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[1].ProtocolMappers[0] = groupMapper("false")
			return c, s
		}, "client squared (a service uses it) own mapper \"groups\" writes short group names"},
		{"value from the request in a scope", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			s = append(s, keycloak.ClientScope{Name: "request", ProtocolMappers: []keycloak.ProtocolMapper{{Name: "asked",
				ProtocolMapper: "oidc-claims-param-value-idtoken-mapper", Config: map[string]string{"claim.name": "groups"}}}})
			c[0].OptionalClientScopes = []string{"request"}
			return c, s
		}, `scope request mapper "asked"`},
		{"nested claim", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[0].ProtocolMappers = append(c[0].ProtocolMappers, keycloak.ProtocolMapper{Name: "attr", ProtocolMapper: "oidc-usermodel-attribute-mapper",
				Config: map[string]string{"claim.name": "groups.extra"}})
			return c, s
		}, "writes the group claim groups.extra"},
		{"unknown type", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[0].ProtocolMappers = append(c[0].ProtocolMappers, keycloak.ProtocolMapper{Name: "js", ProtocolMapper: "script-groups.js"})
			return c, s
		}, "unknown type script-groups.js"},
		{"other client with the audience of a service", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[2].ProtocolMappers = []keycloak.ProtocolMapper{{Name: "to argocd", ProtocolMapper: "oidc-audience-mapper",
				Config: map[string]string{"included.client.audience": "argocd"}}}
			return c, s
		}, `client account (its tokens have the audience argocd) scope microprofile-jwt mapper "groups" (type oidc-usermodel-realm-role-mapper)`},
		{"other client with a custom audience", func(c []keycloak.ClientMappers, s []keycloak.ClientScope) ([]keycloak.ClientMappers, []keycloak.ClientScope) {
			c[2].ProtocolMappers = []keycloak.ProtocolMapper{{Name: "api", ProtocolMapper: "oidc-audience-mapper",
				Config: map[string]string{"included.custom.audience": "squared-api"}}}
			return c, s
		}, "client account (its tokens have the audience squared-api)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, s := realmLikeDevelopment()
			c, s = tt.change(c, s)
			_, err := checkKeycloakMappers(c, s, rulesForDevelopment())
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}
}

func TestCheckKeycloakMappersIgnoreAndWarnings(t *testing.T) {
	c, s := realmLikeDevelopment()
	c[0].ProtocolMappers = append(c[0].ProtocolMappers, keycloak.ProtocolMapper{Name: "x", ProtocolMapper: "custom-checked"})
	c[1].ProtocolMappers = c[1].ProtocolMappers[1:] // No group mapper.
	r := rulesForDevelopment()
	r.Ignore = []string{"custom-checked"}
	warnings, err := checkKeycloakMappers(c, s, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "client squared has no group membership mapper") {
		t.Fatalf("warnings = %q", warnings)
	}

	// A client that the guard does not see is an error.
	r.Clients = append(r.Clients, "harbor")
	if _, err := checkKeycloakMappers(c, s, r); err == nil || !strings.Contains(err.Error(), "does not see the client harbor") {
		t.Fatalf("err = %v", err)
	}
}

type stubClients struct {
	clients []keycloak.ClientMappers
	scopes  []keycloak.ClientScope
}

func (s stubClients) Clients(context.Context) ([]keycloak.ClientMappers, error) {
	return s.clients, nil
}
func (s stubClients) ClientScopes(context.Context) ([]keycloak.ClientScope, error) {
	return s.scopes, nil
}

func TestKeycloakMappersSource(t *testing.T) {
	c, s := realmLikeDevelopment()
	cfg := &FileConfig{Groups: []string{"/squared-admins"}, Sources: []SourceConfig{
		{Type: "keycloak-mappers", Service: "Keycloak", Clients: []string{"argocd", "squared"}, Audiences: []string{"squared-api"}}}}
	sources, err := BuildSources(cfg, nil, stubClients{clients: c, scopes: s})
	if err != nil {
		t.Fatal(err)
	}
	refs, warnings, err := sources[0].References(context.Background())
	if err != nil || len(refs) != 0 || len(warnings) != 0 {
		t.Fatalf("refs %v, warnings %v, err %v", refs, warnings, err)
	}
	// Without clients, the source has nothing to check.
	cfg.Sources[0].Clients = nil
	if _, err := BuildSources(cfg, nil, stubClients{}); err == nil {
		t.Fatal("a source without clients was accepted")
	}
}
