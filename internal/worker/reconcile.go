package worker

import (
	"context"
	"time"

	"squared/internal/domain"
)

// Reconcile processes all tenants and deployments again at an interval,
// until ctx is done. It repairs drift in Git, for example a manual change
// in a repository. Argo CD repairs drift in the cluster. A failure only
// goes to the log: the next run tries again, and the status in the
// database does not change.
func (w *Worker) Reconcile(ctx context.Context, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		start := time.Now()
		tenants, deployments, failures := w.reconcileOnce(ctx)
		w.log.Info("reconcile done", "tenants", tenants, "deployments", deployments,
			"failures", failures, "duration_ms", time.Since(start).Milliseconds())
	}
}

func (w *Worker) reconcileOnce(ctx context.Context) (tenants, deployments, failures int) {
	ts, err := w.store.ListTenants(ctx)
	if err != nil {
		w.log.Error("reconcile: list tenants", "error", err)
		return 0, 0, 1
	}
	for _, t := range ts {
		if t.Status != domain.StatusReady || ctx.Err() != nil {
			continue
		}
		tenants++
		if err := w.syncTenantFiles(ctx, &t); err != nil {
			failures++
			w.log.Warn("reconcile tenant", "tenant", t.Slug, "error", err)
		}
	}
	ds, err := w.store.ListAllDeployments(ctx)
	if err != nil {
		w.log.Error("reconcile: list deployments", "error", err)
		return tenants, 0, failures + 1
	}
	for _, d := range ds {
		switch d.Status {
		case domain.StatusCommitted, domain.StatusDeployed, domain.StatusDegraded:
		default:
			continue // A task owns the deployment now, or the user must act.
		}
		if ctx.Err() != nil {
			break
		}
		deployments++
		if err := w.syncDeployment(ctx, d.ID, 1); err != nil {
			failures++
			w.log.Warn("reconcile deployment", "deployment", d.AppName, "error", err)
		}
	}
	return tenants, deployments, failures
}
