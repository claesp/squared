package api

import (
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
)

// The API has its own types. They keep the API stable when the internal
// types change.

// User is a user.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Admin     bool      `json:"admin"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
}

func userOf(u *domain.User) User {
	return User{ID: u.ID, Email: u.Email, Name: u.Name, Admin: u.Admin, Disabled: u.Disabled, CreatedAt: u.CreatedAt}
}

// Quota is the quota of each environment of a tenant.
type Quota struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Pods   int    `json:"pods"`
}

// Tenant is a tenant.
type Tenant struct {
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	Quota         Quota     `json:"quota"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"status_message,omitempty"`
	Role          string    `json:"role,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func tenantOf(t *domain.Tenant, role domain.Role) Tenant {
	return Tenant{
		Slug: t.Slug, Name: t.Name,
		Quota:  Quota{CPU: t.Quota.CPU, Memory: t.Quota.Memory, Pods: t.Quota.Pods},
		Status: string(t.Status), StatusMessage: t.StatusMessage, Role: string(role),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// Member is a member of a tenant.
type Member struct {
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Environment is an environment of a tenant.
type Environment struct {
	Name          string    `json:"name"`
	Namespace     string    `json:"namespace"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"status_message,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Deployment is a deployment.
type Deployment struct {
	Name          string     `json:"name"`
	Environment   string     `json:"environment"`
	Namespace     string     `json:"namespace"`
	Package       string     `json:"package"`
	Revision      int        `json:"revision"`
	Status        string     `json:"status"`
	StatusMessage string     `json:"status_message,omitempty"`
	ArgoCD        ArgoStatus `json:"argocd"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Revisions     []Revision `json:"revisions,omitempty"`
}

// ArgoStatus is the status that Argo CD reports.
type ArgoStatus struct {
	Application string `json:"application"`
	Sync        string `json:"sync,omitempty"`
	Health      string `json:"health,omitempty"`
	Commit      string `json:"commit,omitempty"`
}

func deploymentOf(d *domain.Deployment) Deployment {
	return Deployment{
		Name: d.Name, Environment: d.Environment, Namespace: d.Namespace, Package: d.PackageName,
		Revision: d.Revision, Status: string(d.Status), StatusMessage: d.StatusMessage,
		ArgoCD:    ArgoStatus{Application: d.AppName, Sync: d.SyncStatus, Health: d.HealthStatus, Commit: d.SyncedCommit},
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// Revision is one version of the inputs of a deployment.
type Revision struct {
	Number         int               `json:"number"`
	PackageVersion string            `json:"package_version"`
	Values         map[string]any    `json:"values"`
	Images         map[string]string `json:"images,omitempty"`
	Commit         string            `json:"commit,omitempty"`
	Status         string            `json:"status"`
	Message        string            `json:"message,omitempty"`
	Error          string            `json:"error,omitempty"`
	CreatedBy      string            `json:"created_by,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

func revisionOf(r *domain.Revision) Revision {
	return Revision{
		Number: r.Number, PackageVersion: r.PackageVersion, Values: r.Values, Images: r.Images,
		Commit: r.CommitSHA, Status: string(r.Status), Message: r.Message, Error: r.Error,
		CreatedBy: r.CreatedByEmail, CreatedAt: r.CreatedAt,
	}
}

func viewOf(v core.DeploymentView) Deployment {
	d := deploymentOf(v.Deployment)
	d.Revisions = make([]Revision, 0, len(v.Revisions))
	for i := range v.Revisions {
		d.Revisions = append(d.Revisions, revisionOf(&v.Revisions[i]))
	}
	return d
}

// PackageSummary is a short description of a package.
type PackageSummary struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

func summaryOf(p *catalog.Package) PackageSummary {
	return PackageSummary{Name: p.Name, Version: p.Version, Title: p.Title, Description: p.Description}
}

// AuditEvent is an entry of the audit log.
type AuditEvent struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Detail string    `json:"detail,omitempty"`
}

// List is the body of all list responses.
type List[T any] struct {
	Items []T `json:"items"`
}

func listOf[T any](items []T) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items}
}
