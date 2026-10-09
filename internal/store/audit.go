package store

import (
	"context"

	"squared/internal/domain"
)

// AddAudit records an audit event.
func (s *Store) AddAudit(ctx context.Context, ev domain.AuditEvent) error {
	_, err := s.db.Exec(ctx, `INSERT INTO audit_events (tenant_id, actor_id, actor_email, action, target, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		nullable(ev.TenantID), nullable(ev.ActorID), ev.ActorEmail, ev.Action, ev.Target, ev.Detail)
	return mapErr(err)
}

// ListAudit returns the newest audit events. If tenantID is "", it returns
// the events of all tenants and of the platform.
func (s *Store) ListAudit(ctx context.Context, tenantID string, limit int) ([]domain.AuditEvent, error) {
	const q = `SELECT id, tenant_id, actor_id, actor_email, action, target, detail, created_at
		FROM audit_events WHERE ($1::uuid IS NULL OR tenant_id = $1) ORDER BY id DESC LIMIT $2`
	rows, err := s.db.Query(ctx, q, nullable(tenantID), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.AuditEvent
	for rows.Next() {
		var ev domain.AuditEvent
		var tenant, actor *string
		if err := rows.Scan(&ev.ID, &tenant, &actor, &ev.ActorEmail, &ev.Action, &ev.Target, &ev.Detail, &ev.CreatedAt); err != nil {
			return nil, mapErr(err)
		}
		ev.TenantID, ev.ActorID = deref(tenant), deref(actor)
		list = append(list, ev)
	}
	return list, mapErr(rows.Err())
}
