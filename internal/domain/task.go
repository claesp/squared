package domain

// TaskKind identifies the work that a task asks the worker to do.
type TaskKind string

// Task kinds. All tasks are idempotent: the worker reads the desired state
// from the database and makes the external systems agree with it. Thus it
// is safe to process a task more than one time.
const (
	TaskTenantSync       TaskKind = "tenant.sync"       // Prepare registry, Git and namespaces of a tenant.
	TaskDeploymentSync   TaskKind = "deployment.sync"   // Render and commit the desired revision.
	TaskDeploymentDelete TaskKind = "deployment.delete" // Remove a deployment.
	TaskTenantDelete     TaskKind = "tenant.delete"     // Remove a tenant without deployments.
	TaskTenantAccess     TaskKind = "tenant.access"     // Copy the members to Keycloak, Harbor and Forgejo.
)

// Task is a message from the API server to the worker.
type Task struct {
	Kind         TaskKind `json:"kind"`
	TenantID     string   `json:"tenant_id,omitempty"`
	DeploymentID string   `json:"deployment_id,omitempty"`
}
