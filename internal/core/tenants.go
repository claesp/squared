package core

import (
	"context"
	"errors"
	"strings"

	"squared/internal/domain"
	"squared/internal/store"
)

// NewTenant is the input to CreateTenant.
type NewTenant struct {
	Slug         string
	Name         string
	OwnerEmail   string
	Environments []string
	Quota        domain.Quota
}

// CreateTenant adds a tenant with its environments and first owner. Only
// platform administrators can add tenants. The worker then prepares the
// external systems.
func (s *Service) CreateTenant(ctx context.Context, actor *domain.User, in NewTenant) (*domain.Tenant, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	var verr domain.ValidationError
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateTenantSlug(in.Slug); err != nil {
		verr.Add("slug", err.Error())
	}
	if err := domain.ValidateDisplayName(in.Name); err != nil {
		verr.Add("name", err.Error())
	}
	if err := domain.ValidateQuota(in.Quota); err != nil {
		var qerr *domain.ValidationError
		if errors.As(err, &qerr) {
			verr.Fields = append(verr.Fields, qerr.Fields...)
		}
	}
	if len(in.Environments) == 0 {
		verr.Add("environments", "at least one environment is necessary")
	}
	seen := map[string]bool{}
	for _, e := range in.Environments {
		if err := domain.ValidateEnvironmentName(e); err != nil {
			verr.Add("environments", e+": "+err.Error())
		}
		if seen[e] {
			verr.Add("environments", e+" is in the list two times")
		}
		seen[e] = true
	}
	email, err := domain.NormalizeEmail(in.OwnerEmail)
	if err != nil {
		verr.Add("owner", err.Error())
	}
	if err := verr.Err(); err != nil {
		return nil, err
	}

	t := &domain.Tenant{Slug: in.Slug, Name: in.Name, Quota: in.Quota, Status: domain.StatusPending}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		owner, err := tx.UserByEmail(ctx, email)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Invalid("owner", "no user has this address")
		}
		if err != nil {
			return err
		}
		if err := tx.CreateTenant(ctx, t); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return domain.Invalid("slug", "a tenant with this slug exists")
			}
			return err
		}
		for _, name := range in.Environments {
			env := &domain.Environment{TenantID: t.ID, Name: name, Namespace: domain.Namespace(t.Slug, name), Status: domain.StatusPending}
			if err := tx.CreateEnvironment(ctx, env); err != nil {
				if errors.Is(err, domain.ErrConflict) {
					return domain.Invalid("environments", "namespace "+env.Namespace+" is already in use")
				}
				return err
			}
		}
		if err := tx.PutMembership(ctx, t.ID, owner.ID, domain.RoleOwner); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: t.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, t.ID, "tenant.create", t.Slug, "owner "+owner.Email)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListTenants returns the tenants that the actor can see.
func (s *Service) ListTenants(ctx context.Context, actor *domain.User) ([]domain.Tenant, error) {
	if actor == nil {
		return nil, domain.ErrUnauthenticated
	}
	if actor.Admin {
		return s.store.ListTenants(ctx)
	}
	return s.store.ListTenantsOfUser(ctx, actor.ID)
}

// Tenant returns a tenant and the role of the actor in it.
func (s *Service) Tenant(ctx context.Context, actor *domain.User, slug string) (Access, error) {
	return s.access(ctx, actor, slug, domain.RoleViewer)
}

// UpdateTenant changes the name and quota of a tenant. Only platform
// administrators can change the quota.
func (s *Service) UpdateTenant(ctx context.Context, actor *domain.User, slug, name string, quota domain.Quota) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	var verr domain.ValidationError
	name = strings.TrimSpace(name)
	if err := domain.ValidateDisplayName(name); err != nil {
		verr.Add("name", err.Error())
	}
	if err := domain.ValidateQuota(quota); err != nil {
		var qerr *domain.ValidationError
		if errors.As(err, &qerr) {
			verr.Fields = append(verr.Fields, qerr.Fields...)
		}
	}
	if err := verr.Err(); err != nil {
		return err
	}
	t := *a.Tenant
	t.Name, t.Quota = name, quota
	return s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.UpdateTenant(ctx, &t); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: t.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, t.ID, "tenant.update", t.Slug,
			"quota cpu="+quota.CPU+" memory="+quota.Memory)
	})
}

// SyncTenant asks the worker to prepare the external systems of a tenant
// again, for example after a failure.
func (s *Service) SyncTenant(ctx context.Context, actor *domain.User, slug string) error {
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.SetTenantStatus(ctx, a.Tenant.ID, domain.StatusPending, ""); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: a.Tenant.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "tenant.sync", a.Tenant.Slug, "")
	})
}

// DeleteTenant marks a tenant for removal. Only platform administrators
// can remove tenants, and only tenants without deployments. The worker
// removes the files of the tenant from the platform repository, archives
// the tenant repository and then removes the tenant from the database.
func (s *Service) DeleteTenant(ctx context.Context, actor *domain.User, slug, confirm string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if confirm != slug {
		return domain.Invalid("confirm", "type the slug of the tenant to confirm the removal")
	}
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		deps, err := tx.ListDeployments(ctx, a.Tenant.ID)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return domain.Invalid("confirm", "remove all deployments of the tenant first")
		}
		if err := tx.SetTenantStatus(ctx, a.Tenant.ID, domain.StatusDeleting, ""); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantDelete, TenantID: a.Tenant.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "tenant.delete", a.Tenant.Slug, "")
	})
}

// ListMembers returns the members of a tenant.
func (s *Service) ListMembers(ctx context.Context, actor *domain.User, slug string) ([]domain.Membership, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.store.ListMemberships(ctx, a.Tenant.ID)
}

// PutMember adds a member or changes the role of a member. Only owners can
// change members. A tenant always keeps at least one owner.
func (s *Service) PutMember(ctx context.Context, actor *domain.User, slug, email string, role domain.Role) error {
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	if !role.Valid() {
		return domain.Invalid("role", "must be owner, developer or viewer")
	}
	email, err = domain.NormalizeEmail(email)
	if err != nil {
		return domain.Invalid("email", err.Error())
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		u, err := tx.UserByEmail(ctx, email)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Invalid("email", "no user has this address")
		}
		if err != nil {
			return err
		}
		old, err := tx.MemberRole(ctx, a.Tenant.ID, u.ID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if old == domain.RoleOwner && role != domain.RoleOwner {
			if err := keepOneOwner(ctx, tx, a.Tenant.ID); err != nil {
				return err
			}
		}
		if err := tx.PutMembership(ctx, a.Tenant.ID, u.ID, role); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "member.put", u.Email, string(role))
	})
}

// RemoveMember removes a member from a tenant.
func (s *Service) RemoveMember(ctx context.Context, actor *domain.User, slug, userID string) error {
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		role, err := tx.MemberRole(ctx, a.Tenant.ID, userID)
		if err != nil {
			return err
		}
		if role == domain.RoleOwner {
			if err := keepOneOwner(ctx, tx, a.Tenant.ID); err != nil {
				return err
			}
		}
		u, err := tx.UserByID(ctx, userID)
		if err != nil {
			return err
		}
		if err := tx.DeleteMembership(ctx, a.Tenant.ID, userID); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "member.remove", u.Email, "")
	})
}

func keepOneOwner(ctx context.Context, tx *store.Store, tenantID string) error {
	n, err := tx.CountOwners(ctx, tenantID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return domain.Invalid("role", "the tenant must keep at least one owner")
	}
	return nil
}

// ListEnvironments returns the environments of a tenant.
func (s *Service) ListEnvironments(ctx context.Context, actor *domain.User, slug string) ([]domain.Environment, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.store.ListEnvironments(ctx, a.Tenant.ID)
}

// CreateEnvironment adds an environment to a tenant.
func (s *Service) CreateEnvironment(ctx context.Context, actor *domain.User, slug, name string) (*domain.Environment, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if err := domain.ValidateEnvironmentName(name); err != nil {
		return nil, domain.Invalid("name", err.Error())
	}
	env := &domain.Environment{TenantID: a.Tenant.ID, Name: name, Namespace: domain.Namespace(a.Tenant.Slug, name), Status: domain.StatusPending}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.CreateEnvironment(ctx, env); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return domain.Invalid("name", "the environment or namespace exists")
			}
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: a.Tenant.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "environment.create", env.Namespace, "")
	})
	if err != nil {
		return nil, err
	}
	return env, nil
}

// DeleteEnvironment removes an environment that has no deployments. The
// worker then removes the namespace.
func (s *Service) DeleteEnvironment(ctx context.Context, actor *domain.User, slug, name string) error {
	a, err := s.access(ctx, actor, slug, domain.RoleOwner)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		env, err := tx.EnvironmentByName(ctx, a.Tenant.ID, name)
		if err != nil {
			return err
		}
		deps, err := tx.ListDeploymentsInEnvironment(ctx, env.ID)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return domain.Invalid("environment", "remove all deployments of the environment first")
		}
		if err := tx.DeleteEnvironment(ctx, env.ID); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: a.Tenant.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "environment.delete", env.Namespace, "")
	})
}

// ListAudit returns the newest audit events of a tenant.
func (s *Service) ListAudit(ctx context.Context, actor *domain.User, slug string, limit int) ([]domain.AuditEvent, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.store.ListAudit(ctx, a.Tenant.ID, limit)
}

// ListPlatformAudit returns the newest audit events of all tenants.
func (s *Service) ListPlatformAudit(ctx context.Context, actor *domain.User, limit int) ([]domain.AuditEvent, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.store.ListAudit(ctx, "", limit)
}
