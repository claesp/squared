package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
)

const deploymentColumns = `d.id, d.tenant_id, d.environment_id, e.name, e.namespace, d.name, d.app_name,
	d.package_name, d.revision, d.status, d.status_message, d.sync_status, d.health_status,
	d.synced_commit, d.created_at, d.updated_at`

const deploymentFrom = " FROM deployments d JOIN environments e ON e.id = d.environment_id "

func scanDeployment(row pgx.Row) (*domain.Deployment, error) {
	var d domain.Deployment
	err := row.Scan(&d.ID, &d.TenantID, &d.EnvironmentID, &d.Environment, &d.Namespace, &d.Name, &d.AppName,
		&d.PackageName, &d.Revision, &d.Status, &d.StatusMessage, &d.SyncStatus, &d.HealthStatus,
		&d.SyncedCommit, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &d, nil
}

func collectDeployments(rows pgx.Rows, err error) ([]domain.Deployment, error) {
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *d)
	}
	return list, mapErr(rows.Err())
}

// CreateDeployment adds a deployment. It sets d.ID, d.CreatedAt and
// d.UpdatedAt.
func (s *Store) CreateDeployment(ctx context.Context, d *domain.Deployment) error {
	err := s.db.QueryRow(ctx, `INSERT INTO deployments
		(tenant_id, environment_id, name, app_name, package_name, revision, status, status_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at, updated_at`,
		d.TenantID, d.EnvironmentID, d.Name, d.AppName, d.PackageName, d.Revision, d.Status, d.StatusMessage).
		Scan(&d.ID, &d.CreatedAt, &d.UpdatedAt)
	return mapErr(err)
}

// DeploymentByID returns a deployment.
func (s *Store) DeploymentByID(ctx context.Context, id string) (*domain.Deployment, error) {
	return scanDeployment(s.db.QueryRow(ctx, "SELECT "+deploymentColumns+deploymentFrom+"WHERE d.id = $1", id))
}

// DeploymentForUpdate returns a deployment and locks its row until the
// transaction ends. Use it to serialize changes to one deployment.
func (s *Store) DeploymentForUpdate(ctx context.Context, id string) (*domain.Deployment, error) {
	return scanDeployment(s.db.QueryRow(ctx, "SELECT "+deploymentColumns+deploymentFrom+
		"WHERE d.id = $1 FOR UPDATE OF d", id))
}

// DeploymentByName returns a deployment of a tenant.
func (s *Store) DeploymentByName(ctx context.Context, tenantID, env, name string) (*domain.Deployment, error) {
	return scanDeployment(s.db.QueryRow(ctx, "SELECT "+deploymentColumns+deploymentFrom+
		"WHERE d.tenant_id = $1 AND e.name = $2 AND d.name = $3", tenantID, env, name))
}

// ListDeployments returns the deployments of a tenant.
func (s *Store) ListDeployments(ctx context.Context, tenantID string) ([]domain.Deployment, error) {
	return collectDeployments(s.db.Query(ctx, "SELECT "+deploymentColumns+deploymentFrom+
		"WHERE d.tenant_id = $1 ORDER BY e.name, d.name", tenantID))
}

// ListDeploymentsInEnvironment returns the deployments of an environment.
func (s *Store) ListDeploymentsInEnvironment(ctx context.Context, envID string) ([]domain.Deployment, error) {
	return collectDeployments(s.db.Query(ctx, "SELECT "+deploymentColumns+deploymentFrom+
		"WHERE d.environment_id = $1 ORDER BY d.name", envID))
}

// ListAllDeployments returns all deployments of all tenants.
func (s *Store) ListAllDeployments(ctx context.Context) ([]domain.Deployment, error) {
	return collectDeployments(s.db.Query(ctx, "SELECT "+deploymentColumns+deploymentFrom+"ORDER BY d.app_name"))
}

// SetDeploymentStatus saves the status of a deployment.
func (s *Store) SetDeploymentStatus(ctx context.Context, id string, st domain.Status, msg string) error {
	return s.execOne(ctx, `UPDATE deployments SET status = $2, status_message = $3, updated_at = now()
		WHERE id = $1`, id, st, msg)
}

// SetDeploymentRevision saves the desired revision of a deployment.
func (s *Store) SetDeploymentRevision(ctx context.Context, id string, revision int, st domain.Status) error {
	return s.execOne(ctx, `UPDATE deployments SET revision = $2, status = $3, status_message = '',
		updated_at = now() WHERE id = $1`, id, revision, st)
}

// SetDeploymentCommitted records that the worker committed a revision. It
// changes nothing if a newer revision is the desired revision now, or if
// the deployment is being deleted.
func (s *Store) SetDeploymentCommitted(ctx context.Context, id string, revision int, msg string) error {
	_, err := s.db.Exec(ctx, `UPDATE deployments SET status = 'committed', status_message = $3, updated_at = now()
		WHERE id = $1 AND revision = $2 AND status <> 'deleting'`, id, revision, msg)
	return mapErr(err)
}

// SyncState is what Argo CD reports about a deployment.
type SyncState struct {
	SyncStatus   string
	HealthStatus string
	Commit       string
	Status       domain.Status
	Message      string
}

// SetDeploymentSync saves the state that Argo CD reports. It changes
// nothing if the deployment is being deleted or the worker did not commit
// its desired revision yet.
func (s *Store) SetDeploymentSync(ctx context.Context, id string, st SyncState) error {
	_, err := s.db.Exec(ctx, `UPDATE deployments SET sync_status = $2, health_status = $3,
			synced_commit = $4, status = $5, status_message = $6, updated_at = now()
		WHERE id = $1 AND status NOT IN ('deleting', 'pending', 'failed')
		  AND (sync_status, health_status, synced_commit, status, status_message) IS DISTINCT FROM ($2, $3, $4, $5, $6)`,
		id, st.SyncStatus, st.HealthStatus, st.Commit, st.Status, st.Message)
	return mapErr(err)
}

// DeleteDeployment removes a deployment and its revisions.
func (s *Store) DeleteDeployment(ctx context.Context, id string) error {
	return s.execOne(ctx, "DELETE FROM deployments WHERE id = $1", id)
}

const revisionColumns = `r.id, r.deployment_id, r.number, r.package_name, r.package_version, r.package,
	r."values", r.images, r.commit_sha, r.status, r.message, r.error, r.created_by, coalesce(u.email, ''), r.created_at`

const revisionFrom = " FROM revisions r LEFT JOIN users u ON u.id = r.created_by "

func scanRevision(row pgx.Row) (*domain.Revision, error) {
	var r domain.Revision
	var values, images []byte
	var createdBy *string
	err := row.Scan(&r.ID, &r.DeploymentID, &r.Number, &r.PackageName, &r.PackageVersion, &r.Package,
		&values, &images, &r.CommitSHA, &r.Status, &r.Message, &r.Error, &createdBy, &r.CreatedByEmail, &r.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	r.CreatedBy = deref(createdBy)
	if err := json.Unmarshal(values, &r.Values); err != nil {
		return nil, fmt.Errorf("revision %s: decode values: %w", r.ID, err)
	}
	if len(images) > 0 {
		if err := json.Unmarshal(images, &r.Images); err != nil {
			return nil, fmt.Errorf("revision %s: decode images: %w", r.ID, err)
		}
	}
	return &r, nil
}

// CreateRevision adds a revision. It sets r.ID and r.CreatedAt.
func (s *Store) CreateRevision(ctx context.Context, r *domain.Revision) error {
	values, err := json.Marshal(r.Values)
	if err != nil {
		return fmt.Errorf("encode values: %w", err)
	}
	var images []byte
	if r.Images != nil {
		if images, err = json.Marshal(r.Images); err != nil {
			return fmt.Errorf("encode images: %w", err)
		}
	}
	err = s.db.QueryRow(ctx, `INSERT INTO revisions
		(deployment_id, number, package_name, package_version, package, "values", images, status, message, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id, created_at`,
		r.DeploymentID, r.Number, r.PackageName, r.PackageVersion, []byte(r.Package), values, images,
		r.Status, r.Message, nullable(r.CreatedBy)).Scan(&r.ID, &r.CreatedAt)
	return mapErr(err)
}

// RevisionByNumber returns a revision of a deployment.
func (s *Store) RevisionByNumber(ctx context.Context, deploymentID string, number int) (*domain.Revision, error) {
	return scanRevision(s.db.QueryRow(ctx, "SELECT "+revisionColumns+revisionFrom+
		"WHERE r.deployment_id = $1 AND r.number = $2", deploymentID, number))
}

// ListRevisions returns the revisions of a deployment, the newest first.
func (s *Store) ListRevisions(ctx context.Context, deploymentID string) ([]domain.Revision, error) {
	rows, err := s.db.Query(ctx, "SELECT "+revisionColumns+revisionFrom+
		"WHERE r.deployment_id = $1 ORDER BY r.number DESC", deploymentID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var list []domain.Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *r)
	}
	return list, mapErr(rows.Err())
}

// SetRevisionImages saves the resolved image references of a revision.
// It changes nothing if the revision already has images. This keeps a
// revision reproducible: the first resolution is final.
func (s *Store) SetRevisionImages(ctx context.Context, id string, images map[string]string) error {
	b, err := json.Marshal(images)
	if err != nil {
		return fmt.Errorf("encode images: %w", err)
	}
	_, err = s.db.Exec(ctx, "UPDATE revisions SET images = $2 WHERE id = $1 AND images IS NULL", id, b)
	return mapErr(err)
}

// SetRevisionResult saves the result of the work on a revision. errMsg
// is "" if the work succeeded.
func (s *Store) SetRevisionResult(ctx context.Context, id, commit string, st domain.Status, errMsg string) error {
	return s.execOne(ctx, "UPDATE revisions SET commit_sha = $2, status = $3, error = $4 WHERE id = $1",
		id, commit, st, errMsg)
}
