package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
)

const tenantColumns = "t.id, t.slug, t.name, t.quota_cpu, t.quota_memory, t.quota_pods, t.status, t.status_message, t.created_at, t.updated_at"

func scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var t domain.Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.Quota.CPU, &t.Quota.Memory, &t.Quota.Pods,
		&t.Status, &t.StatusMessage, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &t, nil
}

func collectTenants(rows pgx.Rows, err error) ([]domain.Tenant, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *t)
	}
	return list, mapErr(rows.Err())
}

// CreateTenant adds a tenant. It sets t.ID, t.CreatedAt and t.UpdatedAt.
func (s *Store) CreateTenant(ctx context.Context, t *domain.Tenant) error {
	err := s.db.QueryRow(ctx, `INSERT INTO tenants (slug, name, quota_cpu, quota_memory, quota_pods, status, status_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at, updated_at`,
		t.Slug, t.Name, t.Quota.CPU, t.Quota.Memory, t.Quota.Pods, t.Status, t.StatusMessage).
		Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	return mapErr(err)
}

// TenantBySlug returns the tenant with a slug.
func (s *Store) TenantBySlug(ctx context.Context, slug string) (*domain.Tenant, error) {
	return scanTenant(s.db.QueryRow(ctx, "SELECT "+tenantColumns+" FROM tenants t WHERE t.slug = $1", slug))
}

// TenantByID returns the tenant with an ID.
func (s *Store) TenantByID(ctx context.Context, id string) (*domain.Tenant, error) {
	return scanTenant(s.db.QueryRow(ctx, "SELECT "+tenantColumns+" FROM tenants t WHERE t.id = $1", id))
}

// ListTenants returns all tenants, ordered by slug.
func (s *Store) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	return collectTenants(s.db.Query(ctx, "SELECT "+tenantColumns+" FROM tenants t ORDER BY t.slug"))
}

// ListTenantsOfUser returns the tenants where a user is a member.
func (s *Store) ListTenantsOfUser(ctx context.Context, userID string) ([]domain.Tenant, error) {
	return collectTenants(s.db.Query(ctx, "SELECT "+tenantColumns+` FROM tenants t
		JOIN memberships m ON m.tenant_id = t.id WHERE m.user_id = $1 ORDER BY t.slug`, userID))
}

// UpdateTenant saves the name and quota of a tenant.
func (s *Store) UpdateTenant(ctx context.Context, t *domain.Tenant) error {
	return s.execOne(ctx, `UPDATE tenants SET name = $2, quota_cpu = $3, quota_memory = $4, quota_pods = $5,
		updated_at = now() WHERE id = $1`, t.ID, t.Name, t.Quota.CPU, t.Quota.Memory, t.Quota.Pods)
}

// SetTenantStatus saves the status of a tenant.
func (s *Store) SetTenantStatus(ctx context.Context, id string, st domain.Status, msg string) error {
	return s.execOne(ctx, `UPDATE tenants SET status = $2, status_message = $3, updated_at = now()
		WHERE id = $1`, id, st, msg)
}

// PutMembership adds a member or changes the role of a member.
func (s *Store) PutMembership(ctx context.Context, tenantID, userID string, role domain.Role) error {
	_, err := s.db.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = excluded.role`, tenantID, userID, role)
	return mapErr(err)
}

// DeleteMembership removes a member.
func (s *Store) DeleteMembership(ctx context.Context, tenantID, userID string) error {
	return s.execOne(ctx, "DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2", tenantID, userID)
}

// MemberRole returns the role of a user in a tenant.
func (s *Store) MemberRole(ctx context.Context, tenantID, userID string) (domain.Role, error) {
	var role domain.Role
	err := s.db.QueryRow(ctx, "SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2",
		tenantID, userID).Scan(&role)
	return role, mapErr(err)
}

// ListMemberships returns the members of a tenant, ordered by email.
func (s *Store) ListMemberships(ctx context.Context, tenantID string) ([]domain.Membership, error) {
	rows, err := s.db.Query(ctx, `SELECT m.tenant_id, m.user_id, u.email, u.name, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 ORDER BY u.email`, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.Membership
	for rows.Next() {
		var m domain.Membership
		if err := rows.Scan(&m.TenantID, &m.UserID, &m.Email, &m.Name, &m.Role, &m.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		list = append(list, m)
	}
	return list, mapErr(rows.Err())
}

// CountOwners returns the number of owners of a tenant.
func (s *Store) CountOwners(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'",
		tenantID).Scan(&n)
	return n, mapErr(err)
}

const environmentColumns = "id, tenant_id, name, namespace, status, status_message, created_at"

func scanEnvironment(row pgx.Row) (*domain.Environment, error) {
	var e domain.Environment
	err := row.Scan(&e.ID, &e.TenantID, &e.Name, &e.Namespace, &e.Status, &e.StatusMessage, &e.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &e, nil
}

// CreateEnvironment adds an environment. It sets e.ID and e.CreatedAt.
func (s *Store) CreateEnvironment(ctx context.Context, e *domain.Environment) error {
	err := s.db.QueryRow(ctx, `INSERT INTO environments (tenant_id, name, namespace, status)
		VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		e.TenantID, e.Name, e.Namespace, e.Status).Scan(&e.ID, &e.CreatedAt)
	return mapErr(err)
}

// EnvironmentByName returns an environment of a tenant.
func (s *Store) EnvironmentByName(ctx context.Context, tenantID, name string) (*domain.Environment, error) {
	return scanEnvironment(s.db.QueryRow(ctx, "SELECT "+environmentColumns+
		" FROM environments WHERE tenant_id = $1 AND name = $2", tenantID, name))
}

// ListEnvironments returns the environments of a tenant, ordered by name.
func (s *Store) ListEnvironments(ctx context.Context, tenantID string) ([]domain.Environment, error) {
	rows, err := s.db.Query(ctx, "SELECT "+environmentColumns+
		" FROM environments WHERE tenant_id = $1 ORDER BY name", tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.Environment
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *e)
	}
	return list, mapErr(rows.Err())
}

// SetEnvironmentStatus saves the status of an environment.
func (s *Store) SetEnvironmentStatus(ctx context.Context, id string, st domain.Status, msg string) error {
	return s.execOne(ctx, "UPDATE environments SET status = $2, status_message = $3 WHERE id = $1", id, st, msg)
}

// DeleteEnvironment removes an environment. It returns domain.ErrConflict
// if the environment has deployments.
func (s *Store) DeleteEnvironment(ctx context.Context, id string) error {
	return s.execOne(ctx, "DELETE FROM environments WHERE id = $1", id)
}
