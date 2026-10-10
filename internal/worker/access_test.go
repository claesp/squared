package worker

import (
	"net/http/httptest"
	"slices"
	"testing"

	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/harbor"
	"squared/internal/keycloak"
	"squared/internal/store/storetest"
)

func TestAccessSync(t *testing.T) {
	e := setup(t)
	kc := newFakeKeycloak(map[string]string{
		"admin@example.com":  "kc-admin",
		"viewer@example.com": "kc-viewer",
	})
	kcSrv := httptest.NewServer(kc)
	t.Cleanup(kcSrv.Close)
	e.w.EnableAccessSync(keycloak.New(kcSrv.URL, "squared", "squared-worker", "worker-secret", kcSrv.Client()))
	id := e.tenantID(t)
	run := func(kind domain.TaskKind) {
		t.Helper()
		storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: kind, TenantID: id}, 1))
	}

	// The tenant sync makes the groups, the Harbor members and the team.
	// The owner (no SSO link) is found in Keycloak by the email address.
	run(domain.TaskTenantSync)
	if got := kc.groupMembers("tenant-acme-owner"); !slices.Equal(got, []string{"kc-admin"}) {
		t.Fatalf("owner group: %v", got)
	}
	if got := kc.groupMembers("tenant-acme-viewer"); got == nil || len(got) != 0 {
		t.Fatalf("viewer group: %v", got)
	}
	wantRoles := map[string]int{"tenant-acme-owner": harbor.RoleMaintainer, "tenant-acme-developer": harbor.RoleDeveloper, "tenant-acme-viewer": harbor.RoleGuest}
	if got := e.harbor.roles("acme"); !mapsEqual(got, wantRoles) {
		t.Fatalf("Harbor members: %v", got)
	}
	team := e.git.team("tenant-acme")
	// "admin" is a reserved name in Forgejo, thus the user is "admin-2".
	if team == nil || !team.repos["squared/tenant-acme"] || !team.members["admin-2"] {
		t.Fatalf("Forgejo team: %+v", team)
	}

	// A developer who signed in with Keycloak, and a viewer without an
	// account in Squared before. A user without a Keycloak account gets
	// the Forgejo access, but no Keycloak group.
	_, err := e.svc.SignInIdentity(e.ctx, core.Identity{Issuer: "http://kc", Subject: "kc-dev", Email: "dev@example.com", EmailVerified: true, Name: "Dev"}, "web")
	storetest.Must(t, err)
	_, err = e.svc.CreateUser(e.ctx, e.owner, core.NewUser{Email: "viewer@example.com", Name: "Viewer", Password: "viewer password 1"})
	storetest.Must(t, err)
	_, err = e.svc.CreateUser(e.ctx, e.owner, core.NewUser{Email: "nokc@example.com", Name: "No Keycloak", Password: "nokc password 12"})
	storetest.Must(t, err)
	storetest.Must(t, e.svc.PutMember(e.ctx, e.owner, "acme", "dev@example.com", domain.RoleDeveloper))
	storetest.Must(t, e.svc.PutMember(e.ctx, e.owner, "acme", "viewer@example.com", domain.RoleViewer))
	storetest.Must(t, e.svc.PutMember(e.ctx, e.owner, "acme", "nokc@example.com", domain.RoleViewer))
	run(domain.TaskTenantAccess)
	if got := kc.groupMembers("tenant-acme-developer"); !slices.Equal(got, []string{"kc-dev"}) {
		t.Fatalf("developer group: %v", got)
	}
	if got := kc.groupMembers("tenant-acme-viewer"); !slices.Equal(got, []string{"kc-viewer"}) {
		t.Fatalf("viewer group: %v", got)
	}
	if !team.members["dev"] || !team.members["viewer"] || !team.members["nokc"] || e.git.users["dev"] != "dev@example.com" {
		t.Fatalf("Forgejo team members: %v", team.members)
	}

	// Squared is the source: a member that somebody added by hand goes.
	kc.mu.Lock()
	kc.members["g-tenant-acme-owner"]["kc-intruder"] = true
	kc.mu.Unlock()
	e.harbor.mu.Lock()
	e.harbor.members["acme"][1].RoleID = harbor.RoleProjectAdmin
	e.harbor.mu.Unlock()
	run(domain.TaskTenantAccess)
	if got := kc.groupMembers("tenant-acme-owner"); !slices.Equal(got, []string{"kc-admin"}) {
		t.Fatalf("owner group after a manual change: %v", got)
	}
	if got := e.harbor.roles("acme"); !mapsEqual(got, wantRoles) {
		t.Fatalf("Harbor members after a manual change: %v", got)
	}

	// A role change moves the user to the other group. A removed member
	// and a disabled user lose the access.
	storetest.Must(t, e.svc.PutMember(e.ctx, e.owner, "acme", "dev@example.com", domain.RoleViewer))
	storetest.Must(t, e.svc.RemoveMember(e.ctx, e.owner, "acme", mustUser(t, e, "nokc@example.com").ID))
	storetest.Must(t, e.svc.SetUserDisabled(e.ctx, e.owner, mustUser(t, e, "viewer@example.com").ID, true))
	run(domain.TaskTenantAccess)
	if got := kc.groupMembers("tenant-acme-developer"); len(got) != 0 {
		t.Fatalf("developer group after the role change: %v", got)
	}
	if got := kc.groupMembers("tenant-acme-viewer"); !slices.Equal(got, []string{"kc-dev"}) {
		t.Fatalf("viewer group after the changes: %v", got)
	}
	if team.members["nokc"] || team.members["viewer"] || !team.members["dev"] {
		t.Fatalf("Forgejo team after the changes: %v", team.members)
	}

	// The removal of the tenant removes the groups, the Harbor members
	// and the team.
	storetest.Must(t, e.svc.DeleteTenant(e.ctx, e.owner, "acme", "acme"))
	run(domain.TaskTenantDelete)
	if kc.groupMembers("tenant-acme-owner") != nil || e.git.team("tenant-acme") != nil || len(e.harbor.roles("acme")) != 0 {
		t.Fatal("access objects stay after the removal of the tenant")
	}
}

func mustUser(t *testing.T, e *env, email string) *domain.User {
	t.Helper()
	u, err := e.w.store.UserByEmail(e.ctx, email)
	storetest.Must(t, err)
	return u
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
