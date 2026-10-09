// Package core has the business rules of Squared. The web interface and
// the API both use the Service, thus both have the same rules and the
// same permission checks.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"squared/internal/catalog"
	"squared/internal/domain"
	"squared/internal/store"
)

// Config is the configuration of a Service.
type Config struct {
	SessionTTL   time.Duration
	DefaultQuota domain.Quota
	// SSO tells that users can sign in with an OpenID Connect provider.
	// Administrators can then add users without a password.
	SSO bool
}

// Service gives access to all operations. All operations that take an
// actor check the permissions of the actor.
type Service struct {
	store   *store.Store
	catalog *catalog.Catalog
	checker catalog.Checker
	log     *slog.Logger
	cfg     Config
}

// New returns a Service.
func New(st *store.Store, cat *catalog.Catalog, checker catalog.Checker, log *slog.Logger, cfg Config) *Service {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	return &Service{store: st, catalog: cat, checker: checker, log: log, cfg: cfg}
}

// Catalog returns the package catalog.
func (s *Service) Catalog() *catalog.Catalog { return s.catalog }

// DefaultQuota returns the quota for new tenants.
func (s *Service) DefaultQuota() domain.Quota { return s.cfg.DefaultQuota }

// Ping checks the database.
func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

// audit records an event. A failure to record does not stop the
// operation, because the data change is already committed. The log keeps
// the event in that case.
func (s *Service) audit(ctx context.Context, st *store.Store, actor *domain.User, tenantID, action, target, detail string) error {
	ev := domain.AuditEvent{TenantID: tenantID, Action: action, Target: target, Detail: detail}
	if actor != nil {
		ev.ActorID, ev.ActorEmail = actor.ID, actor.Email
	} else {
		ev.ActorEmail = "system"
	}
	if err := st.AddAudit(ctx, ev); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	s.log.Info("audit", "actor", ev.ActorEmail, "tenant", tenantID, "action", action, "target", target)
	return nil
}

func requireAdmin(actor *domain.User) error {
	if actor == nil {
		return domain.ErrUnauthenticated
	}
	if !actor.Admin {
		return domain.ErrForbidden
	}
	return nil
}

// Access is a tenant and the role of the actor in it.
type Access struct {
	Tenant *domain.Tenant
	Role   domain.Role
}

// Can tells if the role is the same as or higher than min.
func (a Access) Can(min domain.Role) bool { return a.Role.AtLeast(min) }

// access returns the tenant if the actor has at least the role min. To
// not show which tenants exist, it returns domain.ErrNotFound if the
// actor is not a member. Platform administrators are owners of all
// tenants.
func (s *Service) access(ctx context.Context, actor *domain.User, slug string, min domain.Role) (Access, error) {
	if actor == nil {
		return Access{}, domain.ErrUnauthenticated
	}
	t, err := s.store.TenantBySlug(ctx, slug)
	if err != nil {
		return Access{}, err
	}
	if t.Status == domain.StatusDeleting && min != domain.RoleViewer {
		return Access{}, fmt.Errorf("%w: the tenant is being removed", domain.ErrConflict)
	}
	if actor.Admin {
		return Access{Tenant: t, Role: domain.RoleOwner}, nil
	}
	role, err := s.store.MemberRole(ctx, t.ID, actor.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return Access{}, domain.ErrNotFound
	}
	if err != nil {
		return Access{}, err
	}
	if !role.AtLeast(min) {
		return Access{}, domain.ErrForbidden
	}
	return Access{Tenant: t, Role: role}, nil
}
