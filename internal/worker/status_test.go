package worker

import (
	"testing"

	"squared/internal/domain"
	"squared/internal/gitops"
	"squared/internal/kube"
)

func app(revision, sync, health, target, synced string) *kube.Application {
	a := &kube.Application{}
	a.Metadata.Annotations = map[string]string{gitops.AnnotRevision: revision}
	a.Spec.Source.TargetRevision = target
	a.Status.Sync.Status = sync
	a.Status.Sync.Revision = synced
	a.Status.Health.Status = health
	return a
}

func TestSyncStateOf(t *testing.T) {
	d := &domain.Deployment{Revision: 2}
	tests := []struct {
		name string
		app  *kube.Application
		want domain.Status
	}{
		{"no application", nil, domain.StatusCommitted},
		{"old revision", app("1", "Synced", "Healthy", "c1", "c1"), domain.StatusCommitted},
		{"not synced", app("2", "OutOfSync", "Healthy", "c2", "c1"), domain.StatusCommitted},
		{"synced other commit", app("2", "Synced", "Healthy", "c2", "c1"), domain.StatusCommitted},
		{"progressing", app("2", "Synced", "Progressing", "c2", "c2"), domain.StatusCommitted},
		{"healthy", app("2", "Synced", "Healthy", "c2", "c2"), domain.StatusDeployed},
		{"degraded", app("2", "Synced", "Degraded", "c2", "c2"), domain.StatusDegraded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SyncStateOf(d, tc.app); got.Status != tc.want {
				t.Fatalf("status %s, want %s (%s)", got.Status, tc.want, got.Message)
			}
		})
	}

	failed := app("2", "OutOfSync", "Missing", "c2", "")
	failed.Status.OperationState = &struct {
		Phase   string `json:"phase"`
		Message string `json:"message"`
	}{Phase: "Failed", Message: "forbidden"}
	if got := SyncStateOf(d, failed); got.Status != domain.StatusDegraded || got.Message != "Sync failed: forbidden" {
		t.Fatalf("failed sync: %+v", got)
	}
}
