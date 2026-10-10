package worker

import (
	"errors"
	"net/http/httptest"
	"slices"
	"testing"

	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/forgejo"
	"squared/internal/harbor"
	"squared/internal/keycloak"
	"squared/internal/store/storetest"
)

func TestAccessSync(t *testing.T) {
	e := setup(t)
	kc := newFakeKeycloak(map[string]string{
		"admin@example.com":  "kc-admin",
		"viewer@example.com": "kc-viewer",
		"dev@example.com":    "kc-dev",
	})
	kcSrv := httptest.NewServer(kc)
	t.Cleanup(kcSrv.Close)
	e.w.EnableAccessSync(keycloak.New(kcSrv.URL, "squared", "squared-worker", "worker-secret", kcSrv.Client()), 7)
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
	wantRoles := map[string]int{"/tenant-acme-owner": harbor.RoleMaintainer, "/tenant-acme-developer": harbor.RoleDeveloper, "/tenant-acme-viewer": harbor.RoleGuest}
	if got := e.harbor.roles("acme"); !mapsEqual(got, wantRoles) {
		t.Fatalf("Harbor members: %v", got)
	}
	team := e.git.team("tenant-acme")
	// "admin" is a reserved name in Forgejo, thus the user is "admin-2". The
	// account belongs to the Keycloak user: source 7, login name kc-admin.
	if team == nil || !team.repos["squared/tenant-acme"] || !team.members["admin-2"] || e.git.links["admin-2"] != "7/kc-admin" {
		t.Fatalf("Forgejo team: %+v, links %v", team, e.git.links)
	}

	// A developer who signed in with Keycloak, and a viewer without an
	// account in Squared before. A user without a Keycloak account gets
	// no Keycloak group and no Forgejo account: only Keycloak users can
	// sign in to Forgejo. A local Forgejo account with the name "dev" does
	// not get the access of the Keycloak user dev.
	e.git.mu.Lock()
	e.git.users["dev"] = "local-dev@example.com"
	e.git.mu.Unlock()
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
	if team.members["dev"] || !team.members["dev-2"] || !team.members["viewer"] || team.members["nokc"] ||
		e.git.links["dev-2"] != "7/kc-dev" || e.git.users["dev-2"] != "dev@example.com" {
		t.Fatalf("Forgejo team members: %v, links %v", team.members, e.git.links)
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
	if team.members["nokc"] || team.members["viewer"] || !team.members["dev-2"] {
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

// TestAccessSyncDoesNotTakeAccounts checks that the worker never gives a
// Forgejo account that has the email address of a member, but belongs to
// nobody in Keycloak, to that member.
func TestAccessSyncDoesNotTakeAccounts(t *testing.T) {
	e := setup(t)
	kc := newFakeKeycloak(map[string]string{"admin@example.com": "kc-admin"})
	kcSrv := httptest.NewServer(kc)
	t.Cleanup(kcSrv.Close)
	e.w.EnableAccessSync(keycloak.New(kcSrv.URL, "squared", "squared-worker", "worker-secret", kcSrv.Client()), 7)
	e.git.mu.Lock()
	e.git.users["boss"] = "admin@example.com" // A local account with the address.
	e.git.mu.Unlock()
	err := e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: e.tenantID(t)}, 1)
	if !errors.Is(err, forgejo.ErrEmailInUse) {
		t.Fatalf("err = %v, want ErrEmailInUse", err)
	}
	if team := e.git.team("tenant-acme"); team != nil && team.members["boss"] {
		t.Fatal("the local account got the access of the member")
	}
	if _, ok := e.git.links["boss"]; ok {
		t.Fatal("the local account was linked to the member")
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
