package worker

import (
	"context"
	"strconv"
	"time"

	"squared/internal/domain"
	"squared/internal/gitops"
	"squared/internal/kube"
	"squared/internal/store"
)

// WatchStatus copies the status of the Argo CD applications into the
// database until ctx is done. The web interface reads the status from the
// database, thus the API server needs no access to Kubernetes.
func (w *Worker) WatchStatus(ctx context.Context, interval time.Duration) {
	for {
		if err := w.updateStatus(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("update deployment status", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (w *Worker) updateStatus(ctx context.Context) error {
	apps, err := w.kube.ListApplications(ctx, w.cfg.GitOps.ArgoNamespace, gitops.LabelManaged+"=true")
	if err != nil {
		return err
	}
	byID := make(map[string]*kube.Application, len(apps))
	for i := range apps {
		byID[apps[i].Metadata.Labels[gitops.LabelDeploymentID]] = &apps[i]
	}
	deployments, err := w.store.ListAllDeployments(ctx)
	if err != nil {
		return err
	}
	for _, d := range deployments {
		switch d.Status {
		case domain.StatusCommitted, domain.StatusDeployed, domain.StatusDegraded:
		default:
			continue // The worker did not commit the desired revision yet.
		}
		st := SyncStateOf(&d, byID[d.ID])
		if err := w.store.SetDeploymentSync(ctx, d.ID, st); err != nil {
			return err
		}
	}
	return nil
}

// SyncStateOf maps the state of an Argo CD application to the status of
// a deployment. app is nil if the application does not exist.
func SyncStateOf(d *domain.Deployment, app *kube.Application) store.SyncState {
	if app == nil {
		return store.SyncState{Status: domain.StatusCommitted, Message: "Waiting for Argo CD to make the application"}
	}
	st := store.SyncState{
		SyncStatus:   app.Status.Sync.Status,
		HealthStatus: app.Status.Health.Status,
		Commit:       app.Status.Sync.Revision,
	}
	if app.Metadata.Annotations[gitops.AnnotRevision] != strconv.Itoa(d.Revision) {
		st.Status, st.Message = domain.StatusCommitted, "Waiting for Argo CD to load the new revision"
		return st
	}
	if op := app.Status.OperationState; op != nil && (op.Phase == "Failed" || op.Phase == "Error") {
		st.Status, st.Message = domain.StatusDegraded, "Sync failed: "+op.Message
		return st
	}
	for _, c := range app.Status.Conditions {
		if c.Type == "ComparisonError" || c.Type == "InvalidSpecError" || c.Type == "SyncError" {
			st.Status, st.Message = domain.StatusDegraded, c.Type+": "+c.Message
			return st
		}
	}
	synced := app.Status.Sync.Status == "Synced" && app.Status.Sync.Revision == app.Spec.Source.TargetRevision
	switch {
	case !synced:
		st.Status, st.Message = domain.StatusCommitted, "Argo CD applies the manifests"
	case app.Status.Health.Status == "Healthy":
		st.Status = domain.StatusDeployed
	case app.Status.Health.Status == "Progressing":
		st.Status, st.Message = domain.StatusCommitted, "Pods are starting"
	default:
		st.Status = domain.StatusDegraded
		st.Message = "Health: " + app.Status.Health.Status
		if app.Status.Health.Message != "" {
			st.Message += ": " + app.Status.Health.Message
		}
	}
	return st
}
