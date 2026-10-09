// Package domain holds the core types of Squared and the rules that make
// them valid. It does not depend on other Squared packages.
package domain

import (
	"encoding/json"
	"time"
)

// Status is the life-cycle state of a tenant, environment or deployment.
type Status string

// Status values. Not all values apply to all object types.
const (
	StatusPending      Status = "pending"      // Saved. The worker did not start yet.
	StatusProvisioning Status = "provisioning" // The worker prepares external systems.
	StatusReady        Status = "ready"        // Tenant or environment is ready for use.
	StatusCommitted    Status = "committed"    // Manifests are in Git. Argo CD did not apply them yet.
	StatusDeployed     Status = "deployed"     // Argo CD applied the manifests and they are healthy.
	StatusDegraded     Status = "degraded"     // Argo CD applied the manifests but they are not healthy.
	StatusFailed       Status = "failed"       // The worker cannot complete the operation.
	StatusDeleting     Status = "deleting"     // The worker removes the object.
)

// Done tells if the status is final. Pages that show an object that is not
// done refresh themselves.
func (s Status) Done() bool {
	switch s {
	case StatusReady, StatusDeployed, StatusDegraded, StatusFailed:
		return true
	}
	return false
}

// User is a person who can sign in.
type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash string
	Admin        bool // A platform administrator can do all operations.
	Disabled     bool
	CreatedAt    time.Time
}

// Tenant is an isolated customer of the platform. Each tenant has its own
// namespaces, Git repository, registry project and Argo CD project.
type Tenant struct {
	ID            string
	Slug          string
	Name          string
	Quota         Quota
	Status        Status
	StatusMessage string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Quota is the resource limit for each environment of a tenant.
type Quota struct {
	CPU    string // Kubernetes quantity, for example "4".
	Memory string // Kubernetes quantity, for example "8Gi".
	Pods   int
}

// Membership gives a user a role in a tenant.
type Membership struct {
	TenantID  string
	UserID    string
	Email     string
	Name      string
	Role      Role
	CreatedAt time.Time
}

// Environment is one Kubernetes namespace of a tenant, for example "dev"
// or "prod".
type Environment struct {
	ID            string
	TenantID      string
	Name          string
	Namespace     string
	Status        Status
	StatusMessage string
	CreatedAt     time.Time
}

// Deployment is an instance of a package in an environment.
type Deployment struct {
	ID            string
	TenantID      string
	EnvironmentID string
	Environment   string // Environment name, filled by queries.
	Namespace     string // Environment namespace, filled by queries.
	Name          string
	AppName       string // Name of the Argo CD Application.
	PackageName   string
	Revision      int // The number of the desired revision.
	Status        Status
	StatusMessage string
	SyncStatus    string // Argo CD sync status, for example "Synced".
	HealthStatus  string // Argo CD health status, for example "Healthy".
	SyncedCommit  string // The Git commit that Argo CD applied.
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Revision is one immutable version of the inputs of a deployment. A
// revision contains all data that is necessary to make the same manifests
// again: a copy of the package definition, the parameter values and the
// image digests.
type Revision struct {
	ID             string
	DeploymentID   string
	Number         int
	PackageName    string
	PackageVersion string
	Package        json.RawMessage   // Copy of the package definition.
	Values         map[string]any    // Parameter values from the user.
	Images         map[string]string // Parameter name to image reference with digest.
	CommitSHA      string
	Status         Status
	Message        string // Note from the user.
	Error          string // Last failure of the worker.
	CreatedBy      string
	CreatedByEmail string
	CreatedAt      time.Time
}

// AuditEvent records an operation that changed data.
type AuditEvent struct {
	ID         int64
	TenantID   string
	ActorID    string
	ActorEmail string
	Action     string
	Target     string
	Detail     string
	CreatedAt  time.Time
}

// Session is a signed-in browser session.
type Session struct {
	UserID    string
	CSRFToken string
	ExpiresAt time.Time
}

// APIToken is a personal access token for the API.
type APIToken struct {
	ID         string
	UserID     string
	Name       string
	Prefix     string // The first characters of the token, to help users identify it.
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}
