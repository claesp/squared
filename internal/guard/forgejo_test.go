package guard

import (
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"
)

// forgejo15 returns the configuration of an OAuth source as Forgejo 15
// stores it: with all fields.
func forgejo15() map[string]any {
	return map[string]any{
		"Provider": "openidConnect", "ClientID": "forgejo", "ClientSecret": "top-secret",
		"OpenIDConnectAutoDiscoveryURL": "http://kc/.well-known/openid-configuration", "CustomURLMapping": nil,
		"IconURL": "", "Scopes": nil, "AllowUsernameChange": false, "AttributeSSHPublicKey": "",
		"RequiredClaimName": "", "RequiredClaimValue": "", "GroupClaimName": "groups",
		"AdminGroup": "/squared-admins", "RestrictedGroup": "", "GroupTeamMap": "", "GroupTeamMapRemoval": false,
		"QuotaGroupClaimName": "", "QuotaGroupMap": "", "QuotaGroupMapRemoval": false,
	}
}

// sourceOf makes a row of the view from a configuration, like the view.
func sourceOf(typ int, cfg map[string]any) forgejoSource {
	s := forgejoSource{ID: 1, Name: "keycloak", Type: typ, Active: true}
	s.Keys = slices.Sorted(maps.Keys(cfg))
	str := func(k string) string {
		v, _ := cfg[k].(string)
		return v
	}
	if typ == forgejoOAuthType {
		s.AdminGroup, s.RestrictedGroup, s.GroupTeamMap = str("AdminGroup"), str("RestrictedGroup"), str("GroupTeamMap")
		s.GroupClaimName, s.RequiredClaimName, s.RequiredClaimValue = str("GroupClaimName"), str("RequiredClaimName"), str("RequiredClaimValue")
		s.QuotaGroupClaimName, s.QuotaGroupMap = str("QuotaGroupClaimName"), str("QuotaGroupMap")
	}
	return s
}

func groupsOf(refs []Reference) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.Group)
	}
	sort.Strings(out)
	return out
}

func TestCheckForgejoSourceGroups(t *testing.T) {
	cfg := forgejo15()
	cfg["RestrictedGroup"] = "/guests"
	cfg["GroupTeamMap"] = `{"/ops": {"squared": ["operators"]}}`
	cfg["RequiredClaimName"], cfg["RequiredClaimValue"] = "groups", "/staff"
	cfg["QuotaGroupMap"] = `{"/big": ["large"]}`
	refs, warnings, err := checkForgejoSource(sourceOf(6, cfg), nil)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	if got := groupsOf(refs); !slices.Equal(got, []string{"/big", "/guests", "/ops", "/squared-admins", "/staff"}) {
		t.Fatalf("groups = %q", got)
	}

	// A required claim that is not the groups claim and no group path.
	cfg = forgejo15()
	cfg["RequiredClaimName"], cfg["RequiredClaimValue"] = "email_verified", "true"
	refs, _, err = checkForgejoSource(sourceOf(6, cfg), nil)
	if err != nil || !slices.Equal(groupsOf(refs), []string{"/squared-admins"}) {
		t.Fatalf("groups = %q, err %v", groupsOf(refs), err)
	}
}

func TestCheckForgejoSourceRenamedField(t *testing.T) {
	// An upgrade renames AdminGroup. The value is then in an unknown field,
	// and the known field is missing. Both are errors.
	cfg := forgejo15()
	cfg["AdministratorGroup"] = cfg["AdminGroup"]
	delete(cfg, "AdminGroup")
	_, _, err := checkForgejoSource(sourceOf(6, cfg), nil)
	if err == nil || !strings.Contains(err.Error(), "field AdminGroup is missing") || !strings.Contains(err.Error(), "new field AdministratorGroup can give rights") {
		t.Fatalf("err = %v", err)
	}
	// An operator cannot hide a missing rights field with ignoreFields.
	_, _, err = checkForgejoSource(sourceOf(6, cfg), []string{"AdministratorGroup"})
	if err == nil || !strings.Contains(err.Error(), "field AdminGroup is missing") {
		t.Fatalf("err with ignoreFields = %v", err)
	}
}

func TestCheckForgejoSourceNewFields(t *testing.T) {
	cfg := forgejo15()
	cfg["TeamSyncMode"] = "strict" // A name like a rights field: an error.
	if _, _, err := checkForgejoSource(sourceOf(6, cfg), nil); err == nil || !strings.Contains(err.Error(), "TeamSyncMode") {
		t.Fatalf("err = %v", err)
	}

	cfg = forgejo15()
	cfg["PKCEMethod"] = "S256" // Another name: a warning.
	refs, warnings, err := checkForgejoSource(sourceOf(6, cfg), nil)
	if err != nil || len(refs) != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], "PKCEMethod") {
		t.Fatalf("err %v, refs %v, warnings %v", err, refs, warnings)
	}
	// After the check of an operator, ignoreFields removes the warning.
	if _, warnings, _ = checkForgejoSource(sourceOf(6, cfg), []string{"PKCEMethod"}); len(warnings) != 0 {
		t.Fatalf("warnings with ignoreFields = %v", warnings)
	}

	// A removed field without group meaning: a warning.
	cfg = forgejo15()
	delete(cfg, "IconURL")
	if _, warnings, err := checkForgejoSource(sourceOf(6, cfg), nil); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "IconURL") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

func TestCheckForgejoSourceTypes(t *testing.T) {
	// An OAuth source with another type number: Forgejo changed the types.
	if _, _, err := checkForgejoSource(sourceOf(9, forgejo15()), nil); err == nil || !strings.Contains(err.Error(), "type 9") {
		t.Fatalf("err = %v", err)
	}
	// An LDAP source has other fields and gives no Keycloak groups.
	ldap := map[string]any{"Host": "ldap", "AdminFilter": "(admin=1)", "GroupTeamMap": `{"cn=x": {}}`}
	if refs, _, err := checkForgejoSource(sourceOf(2, ldap), nil); err != nil || len(refs) != 0 {
		t.Fatalf("LDAP: refs %v, err %v", refs, err)
	}
	// An OAuth source whose configuration is not a JSON object.
	if _, _, err := checkForgejoSource(forgejoSource{Name: "x", Type: 6}, nil); err == nil {
		t.Fatal("a configuration without keys was accepted")
	}
}

// cfgJSON is used by the database test.
func cfgJSON(cfg map[string]any) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return string(b)
}
