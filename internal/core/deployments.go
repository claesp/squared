package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"squared/internal/catalog"
	"squared/internal/domain"
	"squared/internal/store"
)

// DeploymentInput is the input to CreateDeployment and UpdateDeployment.
type DeploymentInput struct {
	Environment string         // Only for CreateDeployment.
	Name        string         // Only for CreateDeployment.
	Package     string         // Only for CreateDeployment.
	Version     string         // Package version. "" selects the newest version.
	Values      map[string]any // Parameter values.
	Message     string         // Optional note about the change.
}

// DeploymentView is a deployment with its revisions.
type DeploymentView struct {
	Deployment *domain.Deployment
	Revisions  []domain.Revision // The newest first.
}

// Current returns the desired revision, or nil.
func (v DeploymentView) Current() *domain.Revision {
	for i := range v.Revisions {
		if v.Revisions[i].Number == v.Deployment.Revision {
			return &v.Revisions[i]
		}
	}
	return nil
}

// ListDeployments returns the deployments of a tenant.
func (s *Service) ListDeployments(ctx context.Context, actor *domain.User, slug string) ([]domain.Deployment, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleViewer)
	if err != nil {
		return nil, err
	}
	return s.store.ListDeployments(ctx, a.Tenant.ID)
}

// Deployment returns a deployment and its revisions.
func (s *Service) Deployment(ctx context.Context, actor *domain.User, slug, env, name string) (DeploymentView, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleViewer)
	if err != nil {
		return DeploymentView{}, err
	}
	d, err := s.store.DeploymentByName(ctx, a.Tenant.ID, env, name)
	if err != nil {
		return DeploymentView{}, err
	}
	revs, err := s.store.ListRevisions(ctx, d.ID)
	if err != nil {
		return DeploymentView{}, err
	}
	return DeploymentView{Deployment: d, Revisions: revs}, nil
}

// CreateDeployment adds a deployment and its first revision.
func (s *Service) CreateDeployment(ctx context.Context, actor *domain.User, slug string, in DeploymentInput) (*domain.Deployment, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleDeveloper)
	if err != nil {
		return nil, err
	}
	var verr domain.ValidationError
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateDeploymentName(in.Name); err != nil {
		verr.Add("name", err.Error())
	}
	pkg, ok := s.catalog.Get(in.Package, in.Version)
	if !ok {
		verr.Add("package", "no package has this name and version")
	}
	if err := verr.Err(); err != nil {
		return nil, err
	}
	values, err := pkg.Resolve(in.Values)
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(pkg)
	if err != nil {
		return nil, fmt.Errorf("encode package: %w", err)
	}

	var d *domain.Deployment
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		env, err := tx.EnvironmentByName(ctx, a.Tenant.ID, in.Environment)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Invalid("environment", "the tenant has no such environment")
		}
		if err != nil {
			return err
		}
		d = &domain.Deployment{
			TenantID:      a.Tenant.ID,
			EnvironmentID: env.ID,
			Environment:   env.Name,
			Namespace:     env.Namespace,
			Name:          in.Name,
			AppName:       domain.AppName(a.Tenant.Slug, env.Name, in.Name),
			PackageName:   pkg.Name,
			Revision:      1,
			Status:        domain.StatusPending,
		}
		if err := tx.CreateDeployment(ctx, d); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return domain.Invalid("name", "a deployment with this name exists")
			}
			return err
		}
		rev := &domain.Revision{
			DeploymentID:   d.ID,
			Number:         1,
			PackageName:    pkg.Name,
			PackageVersion: pkg.Version,
			Package:        snapshot,
			Values:         values,
			Status:         domain.StatusPending,
			Message:        truncate(strings.TrimSpace(in.Message), 500),
			CreatedBy:      actor.ID,
		}
		if err := tx.CreateRevision(ctx, rev); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskDeploymentSync, TenantID: a.Tenant.ID, DeploymentID: d.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "deployment.create", d.AppName, pkg.Ref())
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// UpdateDeployment adds a revision with new values or a new package
// version.
func (s *Service) UpdateDeployment(ctx context.Context, actor *domain.User, slug, env, name string, in DeploymentInput) (*domain.Revision, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleDeveloper)
	if err != nil {
		return nil, err
	}
	d, err := s.store.DeploymentByName(ctx, a.Tenant.ID, env, name)
	if err != nil {
		return nil, err
	}
	pkg, ok := s.catalog.Get(d.PackageName, in.Version)
	if !ok {
		return nil, domain.Invalid("version", "the package has no such version")
	}
	values, err := pkg.Resolve(in.Values)
	if err != nil {
		return nil, err
	}
	snapshot, err := json.Marshal(pkg)
	if err != nil {
		return nil, fmt.Errorf("encode package: %w", err)
	}
	rev := &domain.Revision{
		PackageName:    pkg.Name,
		PackageVersion: pkg.Version,
		Package:        snapshot,
		Values:         values,
		Message:        truncate(strings.TrimSpace(in.Message), 500),
	}
	return s.addRevision(ctx, actor, a, d.ID, rev, "deployment.update", pkg.Ref())
}

// Rollback adds a revision that is a copy of an earlier revision. The copy
// has the same package definition, values and image digests, thus the
// result is the same as before.
func (s *Service) Rollback(ctx context.Context, actor *domain.User, slug, env, name string, number int) (*domain.Revision, error) {
	a, err := s.access(ctx, actor, slug, domain.RoleDeveloper)
	if err != nil {
		return nil, err
	}
	d, err := s.store.DeploymentByName(ctx, a.Tenant.ID, env, name)
	if err != nil {
		return nil, err
	}
	old, err := s.store.RevisionByNumber(ctx, d.ID, number)
	if err != nil {
		return nil, err
	}
	if old.Images == nil {
		return nil, domain.Invalid("revision", "the worker did not complete this revision; select a different revision")
	}
	rev := &domain.Revision{
		PackageName:    old.PackageName,
		PackageVersion: old.PackageVersion,
		Package:        old.Package,
		Values:         old.Values,
		Images:         old.Images,
		Message:        fmt.Sprintf("Rollback to revision %d", old.Number),
	}
	return s.addRevision(ctx, actor, a, d.ID, rev, "deployment.rollback", fmt.Sprintf("revision %d", old.Number))
}

func (s *Service) addRevision(ctx context.Context, actor *domain.User, a Access, deploymentID string, rev *domain.Revision, action, detail string) (*domain.Revision, error) {
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		d, err := tx.DeploymentForUpdate(ctx, deploymentID)
		if err != nil {
			return err
		}
		if d.Status == domain.StatusDeleting {
			return fmt.Errorf("%w: the deployment is being deleted", domain.ErrConflict)
		}
		rev.DeploymentID = d.ID
		rev.Number = d.Revision + 1
		rev.Status = domain.StatusPending
		rev.CreatedBy = actor.ID
		if err := tx.CreateRevision(ctx, rev); err != nil {
			return err
		}
		if err := tx.SetDeploymentRevision(ctx, d.ID, rev.Number, domain.StatusPending); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskDeploymentSync, TenantID: a.Tenant.ID, DeploymentID: d.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, action, d.AppName, fmt.Sprintf("revision %d: %s", rev.Number, detail))
	})
	if err != nil {
		return nil, err
	}
	return rev, nil
}

// SyncDeployment asks the worker to process the desired revision again,
// for example after a failure.
func (s *Service) SyncDeployment(ctx context.Context, actor *domain.User, slug, env, name string) error {
	a, err := s.access(ctx, actor, slug, domain.RoleDeveloper)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		d, err := tx.DeploymentByName(ctx, a.Tenant.ID, env, name)
		if err != nil {
			return err
		}
		kind := domain.TaskDeploymentSync
		if d.Status == domain.StatusDeleting {
			kind = domain.TaskDeploymentDelete
		} else if err := tx.SetDeploymentStatus(ctx, d.ID, domain.StatusPending, ""); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: kind, TenantID: a.Tenant.ID, DeploymentID: d.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "deployment.sync", d.AppName, "")
	})
}

// DeleteDeployment marks a deployment for removal. The worker removes the
// manifests from Git and then the database row.
func (s *Service) DeleteDeployment(ctx context.Context, actor *domain.User, slug, env, name string) error {
	a, err := s.access(ctx, actor, slug, domain.RoleDeveloper)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		d, err := tx.DeploymentByName(ctx, a.Tenant.ID, env, name)
		if err != nil {
			return err
		}
		if _, err := tx.DeploymentForUpdate(ctx, d.ID); err != nil {
			return err
		}
		if err := tx.SetDeploymentStatus(ctx, d.ID, domain.StatusDeleting, ""); err != nil {
			return err
		}
		if err := tx.EnqueueTask(ctx, domain.Task{Kind: domain.TaskDeploymentDelete, TenantID: a.Tenant.ID, DeploymentID: d.ID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, a.Tenant.ID, "deployment.delete", d.AppName, "")
	})
}

// PackageOf returns the package definition of a revision.
func (s *Service) PackageOf(rev *domain.Revision) (*catalog.Package, error) {
	return catalog.Parse(rev.Package, s.checker)
}
