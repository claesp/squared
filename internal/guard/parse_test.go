package guard

import (
	"slices"
	"sort"
	"testing"
)

func TestArgoRBACGroups(t *testing.T) {
	csv := `
# Comment
p, role:ops, applications, sync, */*, allow
g, /squared-admins, role:admin
g, "/ops team", role:ops
g,  squared-admins , role:admin
g, ada@example.com, role:readonly
g, role:ops, role:readonly
p, /not-a-g-line, applications, get, */*, allow
`
	groups, others := argoRBACGroups(csv)
	if !slices.Equal(groups, []string{"/squared-admins", "/ops team"}) {
		t.Errorf("groups = %q", groups)
	}
	if !slices.Equal(others, []string{"squared-admins", "ada@example.com"}) {
		t.Errorf("others = %q", others)
	}
}

func TestForgejoInitGroups(t *testing.T) {
	// The format of the init script of the Forgejo Helm chart.
	script := `#!/usr/bin/env bash
  if [[ -z "${AUTH_ID}" ]]; then
    echo "No oauth configuration found with name 'keycloak'. Installing it now..."
    gitea admin auth add-oauth --admin-group "/squared-admins" --auto-discover-url "http://kc/.well-known/openid-configuration" --group-claim-name "groups" --group-team-map "{\"/ops\": {\"squared\": [\"operators\"]}}" --key "${GITEA_OAUTH_KEY_0}" --name "keycloak" --provider "openidConnect" --restricted-group "/guests" --secret "${GITEA_OAUTH_SECRET_0}"
  else
    gitea admin auth update-oauth --id "${AUTH_ID}" --admin-group "/squared-admins" --group-claim-name "groups" --name "keycloak"
  fi
  echo "--admin-group \"/not-a-command\""
`
	got := forgejoInitGroups(script)
	sort.Strings(got)
	want := []string{"/guests", "/ops", "/squared-admins", "/squared-admins"}
	if !slices.Equal(got, want) {
		t.Errorf("groups = %q, want %q", got, want)
	}
}

func TestSplitGroups(t *testing.T) {
	if got := splitGroups(" /a, /b\n/c "); !slices.Equal(got, []string{"/a", "/b", "/c"}) {
		t.Errorf("splitGroups = %q", got)
	}
	if got := splitGroups(""); len(got) != 0 {
		t.Errorf("splitGroups of empty = %q", got)
	}
}

func TestTenantGroupsExpected(t *testing.T) {
	tg := tenantGroups{prefix: "tenant-"}
	if !tg.expected("acme", "/tenant-acme-owner") || !tg.expected("acme", "/tenant-acme-viewer") {
		t.Error("tenant groups of acme are not expected")
	}
	if tg.expected("acme", "/tenant-other-owner") || tg.expected("acme", "/tenant-acme-owner/x") || tg.expected("acme", "tenant-acme-owner") {
		t.Error("a group of another tenant, a subgroup or a short name is expected")
	}
}
