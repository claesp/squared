package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/render"
	"squared/internal/store"
	"squared/internal/store/storetest"
)

type fixture struct {
	svc   *core.Service
	st    *store.Store
	admin *domain.User
	ctx   context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()
	st := storetest.New(t)
	cat, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	storetest.Must(t, err)
	svc := core.New(st, cat, render.Check, storetest.Logger(), core.Config{
		SessionTTL:   time.Hour,
		DefaultQuota: domain.Quota{CPU: "2", Memory: "4Gi", Pods: 20},
	})
	ctx := context.Background()
	created, err := svc.BootstrapAdmin(ctx, "admin@example.com", "admin password 1")
	storetest.Must(t, err)
	if !created {
		t.Fatal("first administrator not made")
	}
	admin, err := svc.Authenticate(ctx, "admin@example.com", "admin password 1")
	storetest.Must(t, err)
	return &fixture{svc: svc, st: st, admin: admin, ctx: ctx}
}

func (f *fixture) user(t *testing.T, email string) *domain.User {
	t.Helper()
	u, err := f.svc.CreateUser(f.ctx, f.admin, core.NewUser{Email: email, Name: email, Password: "password for " + email})
	storetest.Must(t, err)
	return u
}

func (f *fixture) tenant(t *testing.T, slug string, owner *domain.User) *domain.Tenant {
	t.Helper()
	tn, err := f.svc.CreateTenant(f.ctx, f.admin, core.NewTenant{
		Slug: slug, Name: "Tenant " + slug, OwnerEmail: owner.Email,
		Environments: []string{"dev", "prod"}, Quota: f.svc.DefaultQuota(),
	})
	storetest.Must(t, err)
	return tn
}

func isValidation(err error, field string) bool {
	var verr *domain.ValidationError
	return errors.As(err, &verr) && verr.FieldMessage(field) != ""
}

func countTasks(t *testing.T, f *fixture) int {
	t.Helper()
	n, err := f.st.PublishOutbox(f.ctx, 1000, func(context.Context, []byte) error { return nil })
	storetest.Must(t, err)
	return n
}

func TestBootstrapAdminOnlyOnce(t *testing.T) {
	f := setup(t)
	created, err := f.svc.BootstrapAdmin(f.ctx, "other@example.com", "another password")
	storetest.Must(t, err)
	if created {
		t.Fatal("a second first administrator was made")
	}
}

func TestAuthentication(t *testing.T) {
	f := setup(t)
	u := f.user(t, "dev@example.com")
	if _, err := f.svc.Authenticate(f.ctx, "DEV@example.com ", "password for dev@example.com"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, "dev@example.com", "wrong"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, "nobody@example.com", "x"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("unknown user: %v", err)
	}

	token, _, err := f.svc.NewSession(f.ctx, u)
	storetest.Must(t, err)
	got, _, err := f.svc.SessionUser(f.ctx, token)
	storetest.Must(t, err)
	if got.ID != u.ID {
		t.Fatal("session belongs to another user")
	}

	// A disabled user loses the session and cannot sign in.
	storetest.Must(t, f.svc.SetUserDisabled(f.ctx, f.admin, u.ID, true))
	if _, _, err := f.svc.SessionUser(f.ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("session of disabled user: %v", err)
	}
	if _, err := f.svc.Authenticate(f.ctx, "dev@example.com", "password for dev@example.com"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled user signed in: %v", err)
	}
	if err := f.svc.SetUserDisabled(f.ctx, f.admin, f.admin.ID, true); !isValidation(err, "user") {
		t.Fatalf("administrator disabled own account: %v", err)
	}
}

func TestAPIToken(t *testing.T) {
	f := setup(t)
	u := f.user(t, "ci@example.com")
	token, info, err := f.svc.CreateAPIToken(f.ctx, u, "CI", 30)
	storetest.Must(t, err)
	if info.ExpiresAt == nil || len(info.Prefix) != 10 {
		t.Fatalf("unexpected token info %+v", info)
	}
	got, err := f.svc.TokenUser(f.ctx, token)
	storetest.Must(t, err)
	if got.ID != u.ID {
		t.Fatal("token belongs to another user")
	}
	if _, err := f.svc.TokenUser(f.ctx, token+"x"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("bad token accepted: %v", err)
	}
	storetest.Must(t, f.svc.DeleteAPIToken(f.ctx, u, info.ID))
	if _, err := f.svc.TokenUser(f.ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("removed token accepted: %v", err)
	}
}

func TestTenantPermissions(t *testing.T) {
	f := setup(t)
	owner := f.user(t, "owner@example.com")
	dev := f.user(t, "dev@example.com")
	viewer := f.user(t, "viewer@example.com")
	outsider := f.user(t, "outsider@example.com")

	if _, err := f.svc.CreateTenant(f.ctx, owner, core.NewTenant{Slug: "nope"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-administrator made a tenant: %v", err)
	}
	f.tenant(t, "acme", owner)
	if countTasks(t, f) != 1 {
		t.Fatal("tenant creation did not enqueue one task")
	}

	storetest.Must(t, f.svc.PutMember(f.ctx, owner, "acme", dev.Email, domain.RoleDeveloper))
	storetest.Must(t, f.svc.PutMember(f.ctx, owner, "acme", viewer.Email, domain.RoleViewer))

	// Outsiders do not see that the tenant exists.
	if _, err := f.svc.Tenant(f.ctx, outsider, "acme"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	// Viewers can read but not change.
	if _, err := f.svc.ListDeployments(f.ctx, viewer, "acme"); err != nil {
		t.Fatalf("viewer cannot read: %v", err)
	}
	_, err := f.svc.CreateDeployment(f.ctx, viewer, "acme", core.DeploymentInput{Environment: "dev", Name: "web", Package: "web-app", Values: map[string]any{"image": "web:1"}})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer made a deployment: %v", err)
	}
	// Developers cannot change members.
	if err := f.svc.PutMember(f.ctx, dev, "acme", outsider.Email, domain.RoleOwner); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("developer changed members: %v", err)
	}
	// The last owner stays.
	if err := f.svc.PutMember(f.ctx, owner, "acme", owner.Email, domain.RoleViewer); !isValidation(err, "role") {
		t.Fatalf("last owner demoted: %v", err)
	}
	if err := f.svc.RemoveMember(f.ctx, owner, "acme", owner.ID); !isValidation(err, "role") {
		t.Fatalf("last owner removed: %v", err)
	}
}

func TestCreateTenantValidation(t *testing.T) {
	f := setup(t)
	owner := f.user(t, "owner@example.com")
	_, err := f.svc.CreateTenant(f.ctx, f.admin, core.NewTenant{
		Slug: "Bad_Slug", Name: "", OwnerEmail: "not-an-email",
		Environments: []string{"dev", "dev", "has-dash"}, Quota: domain.Quota{CPU: "x", Memory: "4Gi", Pods: 0},
	})
	for _, field := range []string{"slug", "name", "owner", "environments", "quota_cpu", "quota_pods"} {
		if !isValidation(err, field) {
			t.Errorf("no error for %s: %v", field, err)
		}
	}
	f.tenant(t, "acme", owner)
	_, err = f.svc.CreateTenant(f.ctx, f.admin, core.NewTenant{
		Slug: "acme", Name: "Again", OwnerEmail: owner.Email, Environments: []string{"dev"}, Quota: f.svc.DefaultQuota(),
	})
	if !isValidation(err, "slug") {
		t.Fatalf("duplicate slug: %v", err)
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	f := setup(t)
	owner := f.user(t, "owner@example.com")
	f.tenant(t, "acme", owner)
	countTasks(t, f)

	d, err := f.svc.CreateDeployment(f.ctx, owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "shop", Package: "web-app",
		Values: map[string]any{"image": "shop:1.0.0", "replicas": json.Number("3")},
	})
	storetest.Must(t, err)
	if d.AppName != "acme-dev-shop" || d.Revision != 1 || d.Status != domain.StatusPending {
		t.Fatalf("unexpected deployment %+v", d)
	}
	if countTasks(t, f) != 1 {
		t.Fatal("deployment creation did not enqueue one task")
	}

	// Validation of values uses the package.
	_, err = f.svc.CreateDeployment(f.ctx, owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "bad", Package: "web-app", Values: map[string]any{"replicas": json.Number("99")},
	})
	if !isValidation(err, "image") || !isValidation(err, "replicas") {
		t.Fatalf("bad values accepted: %v", err)
	}
	_, err = f.svc.CreateDeployment(f.ctx, owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "shop", Package: "web-app", Values: map[string]any{"image": "x:1"},
	})
	if !isValidation(err, "name") {
		t.Fatalf("duplicate name accepted: %v", err)
	}

	rev, err := f.svc.UpdateDeployment(f.ctx, owner, "acme", "dev", "shop", core.DeploymentInput{
		Values: map[string]any{"image": "shop:1.1.0"}, Message: "new version",
	})
	storetest.Must(t, err)
	if rev.Number != 2 || rev.Values["replicas"] != int64(2) {
		t.Fatalf("unexpected revision %+v", rev)
	}

	// A rollback needs a revision with resolved images.
	if _, err := f.svc.Rollback(f.ctx, owner, "acme", "dev", "shop", 1); !isValidation(err, "revision") {
		t.Fatalf("rollback to unresolved revision: %v", err)
	}
	view, err := f.svc.Deployment(f.ctx, owner, "acme", "dev", "shop")
	storetest.Must(t, err)
	first := view.Revisions[len(view.Revisions)-1]
	storetest.Must(t, f.st.SetRevisionImages(f.ctx, first.ID, map[string]string{"image": "r/acme/shop@sha256:" + strings.Repeat("a", 64)}))
	rb, err := f.svc.Rollback(f.ctx, owner, "acme", "dev", "shop", 1)
	storetest.Must(t, err)
	if rb.Number != 3 || rb.Values["image"] != "shop:1.0.0" || rb.Images == nil {
		t.Fatalf("unexpected rollback revision %+v", rb)
	}

	storetest.Must(t, f.svc.DeleteDeployment(f.ctx, owner, "acme", "dev", "shop"))
	if _, err := f.svc.UpdateDeployment(f.ctx, owner, "acme", "dev", "shop", core.DeploymentInput{
		Values: map[string]any{"image": "shop:2"},
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("update of a deployment that is being deleted: %v", err)
	}
	// An environment with deployments cannot be removed.
	if err := f.svc.DeleteEnvironment(f.ctx, owner, "acme", "dev"); !isValidation(err, "environment") {
		t.Fatalf("environment with deployments removed: %v", err)
	}
	storetest.Must(t, f.svc.DeleteEnvironment(f.ctx, owner, "acme", "prod"))

	events, err := f.svc.ListAudit(f.ctx, owner, "acme", 100)
	storetest.Must(t, err)
	if len(events) < 5 {
		t.Fatalf("expected audit events, got %d", len(events))
	}
}

func TestDeleteTenant(t *testing.T) {
	f := setup(t)
	owner := f.user(t, "owner@example.com")
	f.tenant(t, "acme", owner)
	_, err := f.svc.CreateDeployment(f.ctx, owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "shop", Package: "web-app", Values: map[string]any{"image": "shop:1"},
	})
	storetest.Must(t, err)
	countTasks(t, f)

	if err := f.svc.DeleteTenant(f.ctx, owner, "acme", "acme"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("owner removed a tenant: %v", err)
	}
	if err := f.svc.DeleteTenant(f.ctx, f.admin, "acme", "wrong"); !isValidation(err, "confirm") {
		t.Fatalf("removal without confirmation: %v", err)
	}
	if err := f.svc.DeleteTenant(f.ctx, f.admin, "acme", "acme"); !isValidation(err, "confirm") {
		t.Fatalf("removal of a tenant with deployments: %v", err)
	}

	d, err := f.svc.Deployment(f.ctx, owner, "acme", "dev", "shop")
	storetest.Must(t, err)
	storetest.Must(t, f.st.DeleteDeployment(f.ctx, d.Deployment.ID))
	storetest.Must(t, f.svc.DeleteTenant(f.ctx, f.admin, "acme", "acme"))
	if countTasks(t, f) != 1 {
		t.Fatal("removal did not enqueue one task")
	}
	// Members can still read the tenant, but not change it.
	if _, err := f.svc.Tenant(f.ctx, owner, "acme"); err != nil {
		t.Fatalf("read of a tenant that is being removed: %v", err)
	}
	if _, err := f.svc.CreateEnvironment(f.ctx, owner, "acme", "test"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("change of a tenant that is being removed: %v", err)
	}
}
