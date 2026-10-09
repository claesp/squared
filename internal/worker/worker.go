// Package worker does the work that changes external systems: Harbor,
// Forgejo and Kubernetes. It gets tasks from the queue. Each task handler
// reads the desired state from the database and makes the external systems
// agree with it, thus a handler can run more than one time.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"squared/internal/catalog"
	"squared/internal/domain"
	"squared/internal/forgejo"
	"squared/internal/gitops"
	"squared/internal/harbor"
	"squared/internal/kube"
	"squared/internal/queue"
	"squared/internal/render"
	"squared/internal/rest"
	"squared/internal/store"
)

// Config is the configuration of the worker.
type Config struct {
	GitOps          gitops.Config
	RegistryHost    string // Host name in image references, for example "harbor.example.com".
	ProjectPrefix   string // Prefix of Harbor project names of tenants.
	PlatformProject string // Public Harbor project for images that all tenants can use.
	IngressClass    string
	IngressTLS      bool
}

// Worker handles tasks.
type Worker struct {
	cfg     Config
	store   *store.Store
	forgejo *forgejo.Client
	harbor  *harbor.Client
	kube    *kube.Client
	log     *slog.Logger
}

// New returns a Worker.
func New(cfg Config, st *store.Store, fj *forgejo.Client, hb *harbor.Client, kc *kube.Client, log *slog.Logger) *Worker {
	return &Worker{cfg: cfg, store: st, forgejo: fj, harbor: hb, kube: kc, log: log}
}

// HarborProject returns the Harbor project of a tenant.
func (w *Worker) HarborProject(slug string) string { return w.cfg.ProjectPrefix + slug }

// Prepare makes the objects that all tenants share: the Forgejo
// organization, the platform repository and the platform Harbor project.
func (w *Worker) Prepare(ctx context.Context) error {
	g := w.cfg.GitOps
	if err := w.forgejo.EnsureOrg(ctx, g.Org); err != nil {
		return fmt.Errorf("prepare organization: %w", err)
	}
	if err := w.forgejo.EnsureRepo(ctx, g.Org, g.PlatformRepo, "Squared platform state. Only Squared writes here."); err != nil {
		return fmt.Errorf("prepare platform repository: %w", err)
	}
	marker, err := g.MarkerFile()
	if err != nil {
		return err
	}
	files := map[string][]byte{"tenants/README.md": []byte(platformReadme), gitops.MarkerPath: marker}
	only := func(p string) bool { return p == "tenants/README.md" || p == gitops.MarkerPath }
	if _, _, err := w.forgejo.Sync(ctx, g.Org, g.PlatformRepo, g.Branch, only, files, "Prepare the platform repository"); err != nil {
		return fmt.Errorf("prepare platform repository: %w", err)
	}
	if w.cfg.PlatformProject != "" {
		if _, err := w.harbor.EnsureProject(ctx, w.cfg.PlatformProject, true); err != nil {
			return fmt.Errorf("prepare platform project: %w", err)
		}
	}
	return nil
}

const platformReadme = `# Tenants

Argo CD applies this directory with the root application "squared-platform".
The Squared worker writes all files. Do not change them by hand: the worker
replaces manual changes at the next synchronization.
`

// Handle processes a task. It has the type queue.Handler.
func (w *Worker) Handle(ctx context.Context, task domain.Task, attempt int) error {
	// Only one task for each object runs at the same time. Without the
	// lock, a sync that started before a removal can commit the files
	// again after the removal.
	switch task.Kind {
	case domain.TaskTenantSync:
		return w.store.WithLock(ctx, "tenant:"+task.TenantID, func() error { return w.syncTenant(ctx, task.TenantID) })
	case domain.TaskTenantDelete:
		return w.store.WithLock(ctx, "tenant:"+task.TenantID, func() error { return w.deleteTenant(ctx, task.TenantID) })
	case domain.TaskDeploymentSync:
		return w.store.WithLock(ctx, "deployment:"+task.DeploymentID, func() error { return w.syncDeployment(ctx, task.DeploymentID, attempt) })
	case domain.TaskDeploymentDelete:
		return w.store.WithLock(ctx, "deployment:"+task.DeploymentID, func() error { return w.deleteDeployment(ctx, task.DeploymentID) })
	}
	return queue.Permanent(fmt.Errorf("unknown task kind %q", task.Kind))
}

// GiveUp records the failure of a task.
func (w *Worker) GiveUp(ctx context.Context, task domain.Task, cause error) {
	msg := failureMessage(cause)
	var err error
	switch task.Kind {
	case domain.TaskTenantSync:
		err = w.store.SetTenantStatus(ctx, task.TenantID, domain.StatusFailed, msg)
	case domain.TaskDeploymentSync:
		err = w.failDeployment(ctx, task.DeploymentID, msg)
	case domain.TaskDeploymentDelete:
		err = w.store.SetDeploymentStatus(ctx, task.DeploymentID, domain.StatusDeleting, "Removal failed: "+msg)
	case domain.TaskTenantDelete:
		err = w.store.SetTenantStatus(ctx, task.TenantID, domain.StatusDeleting, "Removal failed: "+msg)
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		w.log.Error("record task failure", "kind", task.Kind, "error", err)
	}
}

func failureMessage(err error) string {
	msg := err.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}

func (w *Worker) failDeployment(ctx context.Context, deploymentID, msg string) error {
	d, err := w.store.DeploymentByID(ctx, deploymentID)
	if err != nil {
		return err
	}
	rev, err := w.store.RevisionByNumber(ctx, d.ID, d.Revision)
	if err != nil {
		return err
	}
	if err := w.store.SetRevisionResult(ctx, rev.ID, rev.CommitSHA, domain.StatusFailed, msg); err != nil {
		return err
	}
	return w.store.SetDeploymentStatus(ctx, d.ID, domain.StatusFailed, msg)
}

// syncTenant prepares the registry project, the Git repository and the
// namespaces of a tenant.
func (w *Worker) syncTenant(ctx context.Context, tenantID string) error {
	t, err := w.store.TenantByID(ctx, tenantID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // The tenant was removed. Nothing to do.
	}
	if err != nil {
		return err
	}
	if t.Status == domain.StatusDeleting {
		return nil // The delete task owns the tenant now.
	}
	envs, err := w.store.ListEnvironments(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := w.store.SetTenantStatus(ctx, t.ID, domain.StatusProvisioning, "Preparing registry, repository and namespaces"); err != nil {
		return err
	}
	if err := w.syncTenantFiles(ctx, t); err != nil {
		return err
	}
	for _, e := range envs {
		if err := w.store.SetEnvironmentStatus(ctx, e.ID, domain.StatusReady, ""); err != nil {
			return err
		}
	}
	return w.store.SetTenantStatus(ctx, t.ID, domain.StatusReady, "")
}

// syncTenantFiles makes the registry project, the Git repository, the
// platform files and the pull secrets of a tenant agree with the database.
// It does not change the status of the tenant.
func (w *Worker) syncTenantFiles(ctx context.Context, t *domain.Tenant) error {
	envs, err := w.store.ListEnvironments(ctx, t.ID)
	if err != nil {
		return err
	}
	g := w.cfg.GitOps
	project, err := w.harbor.EnsureProject(ctx, w.HarborProject(t.Slug), false)
	if err != nil {
		return fmt.Errorf("registry project: %w", err)
	}
	if err := w.forgejo.EnsureRepo(ctx, g.Org, gitops.TenantRepo(t.Slug), "Manifests of tenant "+t.Slug+". Only Squared writes here."); err != nil {
		return fmt.Errorf("tenant repository: %w", err)
	}
	files, err := g.TenantFiles(t, envs)
	if err != nil {
		return queue.Permanent(err)
	}
	msg := fmt.Sprintf("Tenant %s: namespaces %s", t.Slug, envNames(envs))
	if _, _, err := w.forgejo.Sync(ctx, g.Org, g.PlatformRepo, g.Branch, gitops.TenantScope(t.Slug), files, msg); err != nil {
		return fmt.Errorf("platform repository: %w", err)
	}

	// Argo CD makes the namespaces some seconds after the commit. Until
	// then, the pull secrets cannot be written and the task is retried.
	return w.ensurePullSecrets(ctx, t, project, envs)
}

func envNames(envs []domain.Environment) string {
	names := make([]string, 0, len(envs))
	for _, e := range envs {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}

// ensurePullSecrets writes the image pull secret into each namespace that
// does not have it. Harbor shows a robot secret only one time, thus a new
// secret replaces the secret in all namespaces.
func (w *Worker) ensurePullSecrets(ctx context.Context, t *domain.Tenant, project *harbor.Project, envs []domain.Environment) error {
	missing := false
	for _, e := range envs {
		ok, err := w.kube.SecretExists(ctx, e.Namespace, render.PullSecretName)
		// Status 403 tells that Argo CD did not make the RoleBinding yet.
		// The write below then fails, and the task is retried.
		if err != nil && !rest.HasStatus(err, http.StatusForbidden) {
			return fmt.Errorf("check pull secret in %s: %w", e.Namespace, err)
		}
		if !ok {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	robot, err := w.harbor.PullRobot(ctx, project, "pull")
	if err != nil {
		return fmt.Errorf("registry robot: %w", err)
	}
	labels := map[string]string{render.LabelManagedBy: render.ManagedBy, render.LabelTenant: t.Slug}
	for _, e := range envs {
		err := w.kube.ApplyPullSecret(ctx, e.Namespace, render.PullSecretName, w.cfg.RegistryHost, robot.Name, robot.Secret, labels)
		if err != nil {
			if serr := w.store.SetEnvironmentStatus(ctx, e.ID, domain.StatusProvisioning, "Waiting for the namespace"); serr != nil {
				return serr
			}
			return fmt.Errorf("pull secret in %s: %w", e.Namespace, err)
		}
	}
	return nil
}

// syncDeployment renders the desired revision, commits the manifests to
// the tenant repository and the application to the platform repository.
func (w *Worker) syncDeployment(ctx context.Context, deploymentID string, attempt int) error {
	d, err := w.store.DeploymentByID(ctx, deploymentID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.Status == domain.StatusDeleting {
		return nil // The delete task owns the deployment now.
	}
	t, err := w.store.TenantByID(ctx, d.TenantID)
	if err != nil {
		return err
	}
	rev, err := w.store.RevisionByNumber(ctx, d.ID, d.Revision)
	if err != nil {
		return err
	}
	pkg, err := catalog.Parse(rev.Package, render.Check)
	if err != nil {
		return queue.Permanent(fmt.Errorf("package of revision %d: %w", rev.Number, err))
	}
	values, err := pkg.Resolve(rev.Values)
	if err != nil {
		return queue.Permanent(fmt.Errorf("values of revision %d: %w", rev.Number, err))
	}

	if rev.Images == nil {
		images, err := w.resolveImages(ctx, t, pkg, values)
		if err != nil {
			if errors.Is(err, harbor.ErrImageNotFound) && attempt >= 3 {
				return queue.Permanent(err)
			}
			return err
		}
		if err := w.store.SetRevisionImages(ctx, rev.ID, images); err != nil {
			return err
		}
		// Read again: if two workers resolved at the same time, the
		// first result is final.
		if rev, err = w.store.RevisionByNumber(ctx, d.ID, d.Revision); err != nil {
			return err
		}
	}

	files, err := render.Render(render.Input{
		Tenant:       t.Slug,
		Namespace:    d.Namespace,
		Deployment:   d.Name,
		Package:      pkg,
		Values:       values,
		Images:       rev.Images,
		IngressClass: w.cfg.IngressClass,
		IngressTLS:   w.cfg.IngressTLS,
	})
	if err != nil {
		return queue.Permanent(err)
	}
	dir := gitops.DeploymentDir(d.Environment, d.Name)
	desired := make(map[string][]byte, len(files))
	for _, f := range files {
		desired[dir+"/"+f.Path] = f.Content
	}
	g := w.cfg.GitOps
	who := rev.CreatedByEmail
	if who == "" {
		who = "unknown"
	}
	msg := fmt.Sprintf("Deploy %s revision %d\n\nPackage: %s@%s\nChanged by: %s\n", d.AppName, rev.Number, rev.PackageName, rev.PackageVersion, who)
	if rev.Message != "" {
		msg += "Note: " + rev.Message + "\n"
	}
	commit, changedManifests, err := w.forgejo.Sync(ctx, g.Org, gitops.TenantRepo(t.Slug), g.Branch, forgejo.Prefix(dir), desired, msg)
	if err != nil {
		return fmt.Errorf("tenant repository: %w", err)
	}
	// Without a change, Sync returns the head of the branch. Commits of
	// other deployments move the head. The files of this deployment are
	// the same in the commit of the revision, thus keep that commit: each
	// revision maps to exactly one commit.
	if !changedManifests && rev.CommitSHA != "" {
		commit = rev.CommitSHA
	}

	appPath, app, err := g.ApplicationFile(gitops.AppInput{Tenant: t, Deployment: d, Commit: commit, Revision: rev.Number})
	if err != nil {
		return queue.Permanent(err)
	}
	only := func(p string) bool { return p == appPath }
	_, changedApp, err := w.forgejo.Sync(ctx, g.Org, g.PlatformRepo, g.Branch, only, map[string][]byte{appPath: app}, msg)
	if err != nil {
		return fmt.Errorf("platform repository: %w", err)
	}

	// Without a change in Git, a deployment that Argo CD already reports
	// keeps its status. This is the normal case for a reconcile run.
	unchanged := !changedManifests && !changedApp && rev.Status == domain.StatusCommitted && rev.CommitSHA == commit
	if unchanged && (d.Status == domain.StatusDeployed || d.Status == domain.StatusDegraded || d.Status == domain.StatusCommitted) {
		return nil
	}
	if err := w.store.SetRevisionResult(ctx, rev.ID, commit, domain.StatusCommitted, ""); err != nil {
		return err
	}
	return w.store.SetDeploymentCommitted(ctx, d.ID, rev.Number, "Waiting for Argo CD")
}

// resolveImages finds the digest of each image. A digest makes the
// revision reproducible: a tag can move, a digest cannot.
func (w *Worker) resolveImages(ctx context.Context, t *domain.Tenant, pkg *catalog.Package, values map[string]any) (map[string]string, error) {
	images := map[string]string{}
	for _, prm := range pkg.Parameters {
		if prm.Type != catalog.TypeImage {
			continue
		}
		v, ok := values[prm.Name].(string)
		if !ok {
			continue
		}
		img, err := catalog.ParseImage(v)
		if err != nil {
			return nil, queue.Permanent(fmt.Errorf("parameter %s: %w", prm.Name, err))
		}
		project := w.HarborProject(t.Slug)
		if prm.Project != "" {
			project = prm.Project
		}
		ref := img.Tag
		if img.Digest != "" {
			ref = img.Digest
		}
		digest, err := w.harbor.Digest(ctx, project, img.Repository, ref)
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", prm.Name, err)
		}
		images[prm.Name] = w.cfg.RegistryHost + "/" + project + "/" + img.Repository + "@" + digest
	}
	return images, nil
}

// deleteDeployment removes the application and the manifests from Git.
// It removes the database row after Argo CD removed the application and
// its objects.
func (w *Worker) deleteDeployment(ctx context.Context, deploymentID string) error {
	d, err := w.store.DeploymentByID(ctx, deploymentID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.Status != domain.StatusDeleting {
		return nil // The user did not ask for removal.
	}
	t, err := w.store.TenantByID(ctx, d.TenantID)
	if err != nil {
		return err
	}
	g := w.cfg.GitOps
	msg := fmt.Sprintf("Remove %s", d.AppName)
	appPath := gitops.AppFile(t.Slug, d.Environment, d.Name)
	only := func(p string) bool { return p == appPath }
	if _, _, err := w.forgejo.Sync(ctx, g.Org, g.PlatformRepo, g.Branch, only, nil, msg); err != nil {
		return fmt.Errorf("platform repository: %w", err)
	}
	dir := gitops.DeploymentDir(d.Environment, d.Name)
	if _, _, err := w.forgejo.Sync(ctx, g.Org, gitops.TenantRepo(t.Slug), g.Branch, forgejo.Prefix(dir), nil, msg); err != nil {
		return fmt.Errorf("tenant repository: %w", err)
	}
	exists, err := w.kube.ApplicationExists(ctx, g.ArgoNamespace, d.AppName)
	if err != nil {
		return err
	}
	if exists {
		if err := w.store.SetDeploymentStatus(ctx, d.ID, domain.StatusDeleting, "Waiting for Argo CD to remove the objects"); err != nil {
			return err
		}
		return errors.New("argo cd did not remove the application yet")
	}
	return w.store.DeleteDeployment(ctx, d.ID)
}

// deleteTenant removes the files of a tenant from the platform repository
// and archives the tenant repository. Argo CD then removes the project,
// the quotas and the RBAC objects. The namespaces and the Harbor project
// stay: an administrator removes them after a check.
func (w *Worker) deleteTenant(ctx context.Context, tenantID string) error {
	t, err := w.store.TenantByID(ctx, tenantID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Status != domain.StatusDeleting {
		return nil
	}
	deps, err := w.store.ListDeployments(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(deps) > 0 {
		return queue.Permanent(fmt.Errorf("tenant %s has deployments", t.Slug))
	}
	g := w.cfg.GitOps
	msg := fmt.Sprintf("Remove tenant %s", t.Slug)
	if _, _, err := w.forgejo.Sync(ctx, g.Org, g.PlatformRepo, g.Branch, forgejo.Prefix(gitops.TenantDir(t.Slug)), nil, msg); err != nil {
		return fmt.Errorf("platform repository: %w", err)
	}
	if err := w.forgejo.ArchiveRepo(ctx, g.Org, gitops.TenantRepo(t.Slug)); err != nil {
		return fmt.Errorf("archive tenant repository: %w", err)
	}
	return w.store.DeleteTenant(ctx, t.ID)
}
