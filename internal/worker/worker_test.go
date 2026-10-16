package worker

import (
	"context"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/forgejo"
	"squared/internal/gitops"
	"squared/internal/harbor"
	"squared/internal/queue"
	"squared/internal/render"
	"squared/internal/store"
	"squared/internal/store/storetest"
)

const digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const digest2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

type env struct {
	ctx    context.Context
	svc    *core.Service
	w      *Worker
	git    *fakeForgejo
	harbor *fakeHarbor
	owner  *domain.User
}

func setup(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	cat, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	storetest.Must(t, err)
	log := storetest.Logger()
	svc := core.New(st, cat, render.Check, log, core.Config{SessionTTL: time.Hour})
	ctx := context.Background()
	_, err = svc.BootstrapAdmin(ctx, "admin@example.com", "admin password 1")
	storetest.Must(t, err)
	admin, err := svc.Authenticate(ctx, "admin@example.com", "admin password 1")
	storetest.Must(t, err)

	git := newFakeForgejo()
	gitSrv := httptest.NewServer(git)
	t.Cleanup(gitSrv.Close)
	hb := &fakeHarbor{projects: map[string]int64{}, tags: map[string]string{"acme/shop:1.0": digest1}}
	hbSrv := httptest.NewServer(hb)
	t.Cleanup(hbSrv.Close)
	kubeSrv := httptest.NewTLSServer(&fakeKube{secrets: map[string]bool{}})
	t.Cleanup(kubeSrv.Close)

	cfg := Config{
		GitOps: gitops.Config{
			Org: "squared", PlatformRepo: "platform", Branch: "main", RepoURLBase: "http://git",
			ArgoNamespace: "argocd", ClusterServer: "https://kubernetes.default.svc",
			WorkerServiceAccount: "squared-worker", WorkerNamespace: "squared", WorkerClusterRole: "squared-worker-pull-secrets",
		},
		RegistryHost: "registry.test",
	}
	w := New(cfg, st, forgejo.New(gitSrv.URL, "token", gitSrv.Client()),
		harbor.New(hbSrv.URL, "admin", "pw", hbSrv.Client()), newKubeClient(t, kubeSrv), log)
	storetest.Must(t, w.Prepare(ctx))

	_, err = svc.CreateTenant(ctx, admin, core.NewTenant{
		Slug: "acme", Name: "Acme", OwnerEmail: admin.Email, Environments: []string{"dev"},
		Quota: domain.Quota{CPU: "1", Memory: "1Gi", Pods: 5},
	})
	storetest.Must(t, err)
	return &env{ctx: ctx, svc: svc, w: w, git: git, harbor: hb, owner: admin}
}

func (e *env) tenantID(t *testing.T) string {
	a, err := e.svc.Tenant(e.ctx, e.owner, "acme")
	storetest.Must(t, err)
	return a.Tenant.ID
}

func (e *env) deployment(t *testing.T) core.DeploymentView {
	v, err := e.svc.Deployment(e.ctx, e.owner, "acme", "dev", "shop")
	storetest.Must(t, err)
	return v
}

func TestTenantSync(t *testing.T) {
	e := setup(t)
	id := e.tenantID(t)
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: id}, 1))

	a, err := e.svc.Tenant(e.ctx, e.owner, "acme")
	storetest.Must(t, err)
	if a.Tenant.Status != domain.StatusReady {
		t.Fatalf("tenant status %s: %s", a.Tenant.Status, a.Tenant.StatusMessage)
	}
	if e.git.file("squared/platform", "tenants/acme/dev/namespace.json") == nil {
		t.Fatal("namespace file is missing")
	}
	if e.git.file("squared/platform", gitops.MarkerPath) == nil {
		t.Fatal("the marker manifest is missing")
	}
	if _, ok := e.harbor.projects["acme"]; !ok {
		t.Fatal("Harbor project is missing")
	}

	// A second run changes nothing.
	commits := e.git.commitCount("squared/platform")
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: id}, 1))
	if e.git.commitCount("squared/platform") != commits {
		t.Fatal("a second tenant sync made a commit")
	}
}

func TestDeploymentSync(t *testing.T) {
	e := setup(t)
	id := e.tenantID(t)
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: id}, 1))

	d, err := e.svc.CreateDeployment(e.ctx, e.owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "shop", Package: "web-app", Values: map[string]any{"image": "shop:1.0"},
	})
	storetest.Must(t, err)
	task := domain.Task{Kind: domain.TaskDeploymentSync, TenantID: id, DeploymentID: d.ID}
	storetest.Must(t, e.w.Handle(e.ctx, task, 1))

	v := e.deployment(t)
	rev := v.Current()
	if v.Deployment.Status != domain.StatusCommitted || rev.Status != domain.StatusCommitted || len(rev.CommitSHA) != 40 {
		t.Fatalf("after sync: deployment %s, revision %s, commit %q", v.Deployment.Status, rev.Status, rev.CommitSHA)
	}
	if rev.Images["image"] != "registry.test/acme/shop@"+digest1 {
		t.Fatalf("image %q", rev.Images["image"])
	}
	manifest := string(e.git.file("squared/tenant-acme", "dev/shop/deployment-shop-web.json"))
	if !strings.Contains(manifest, digest1) {
		t.Fatal("the manifest does not use the digest")
	}
	app := string(e.git.file("squared/platform", "tenants/acme/apps/dev-shop.json"))
	if !strings.Contains(app, `"targetRevision": "`+rev.CommitSHA+`"`) {
		t.Fatalf("the application does not follow the commit of the revision:\n%s", app)
	}

	// The tag moves to a new image. The revision keeps the first digest.
	e.harbor.tags["acme/shop:1.0"] = digest2
	storetest.Must(t, e.w.Handle(e.ctx, task, 1))
	if got := e.deployment(t).Current().Images["image"]; !strings.HasSuffix(got, digest1) {
		t.Fatalf("the digest changed to %s", got)
	}

	// Argo CD reports the deployment. Another deployment then moves the
	// head of the tenant repository. Reconcile must not change the
	// commit or the status of the first deployment.
	_, err = e.svc.CreateDeployment(e.ctx, e.owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "other", Package: "worker", Values: map[string]any{"image": "shop:1.0"},
	})
	storetest.Must(t, err)
	other, err := e.svc.Deployment(e.ctx, e.owner, "acme", "dev", "other")
	storetest.Must(t, err)
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskDeploymentSync, TenantID: id, DeploymentID: other.Deployment.ID}, 1))
	storetest.Must(t, e.w.store.SetDeploymentSync(e.ctx, d.ID, syncedState(rev.CommitSHA)))
	platformCommits := e.git.commitCount("squared/platform")

	_, _, failures := e.w.reconcileOnce(e.ctx)
	if failures != 0 {
		t.Fatalf("reconcile had %d failures", failures)
	}
	if e.git.commitCount("squared/platform") != platformCommits {
		t.Fatal("reconcile made a commit in the platform repository")
	}
	after := e.deployment(t)
	if after.Deployment.Status != domain.StatusDeployed || after.Current().CommitSHA != rev.CommitSHA {
		t.Fatalf("reconcile changed the deployment: status %s, commit %s", after.Deployment.Status, after.Current().CommitSHA)
	}

	// A manual change in Git is repaired.
	e.git.mu.Lock()
	head := e.git.head("squared/tenant-acme")
	files := map[string][]byte{}
	for k, v := range head.files {
		files[k] = v
	}
	files["dev/shop/deployment-shop-web.json"] = []byte("{}\n")
	e.git.repos["squared/tenant-acme"] = append(e.git.repos["squared/tenant-acme"], fakeCommit{sha: e.git.newSHA(), files: files})
	e.git.mu.Unlock()
	e.w.reconcileOnce(e.ctx)
	if string(e.git.file("squared/tenant-acme", "dev/shop/deployment-shop-web.json")) != manifest {
		t.Fatal("reconcile did not repair the manual change")
	}

	// Removal deletes the files and then the database row.
	storetest.Must(t, e.svc.DeleteDeployment(e.ctx, e.owner, "acme", "dev", "shop"))
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskDeploymentDelete, TenantID: id, DeploymentID: d.ID}, 1))
	if e.git.file("squared/platform", "tenants/acme/apps/dev-shop.json") != nil {
		t.Fatal("the application file is still there")
	}
	if _, err := e.svc.Deployment(e.ctx, e.owner, "acme", "dev", "shop"); err == nil {
		t.Fatal("the deployment is still in the database")
	}
}

func TestMissingImageFailsPermanently(t *testing.T) {
	e := setup(t)
	id := e.tenantID(t)
	d, err := e.svc.CreateDeployment(e.ctx, e.owner, "acme", core.DeploymentInput{
		Environment: "dev", Name: "shop", Package: "web-app", Values: map[string]any{"image": "missing:1.0"},
	})
	storetest.Must(t, err)
	task := domain.Task{Kind: domain.TaskDeploymentSync, TenantID: id, DeploymentID: d.ID}
	if err := e.w.Handle(e.ctx, task, 1); err == nil || isPermanent(err) {
		t.Fatalf("first attempt: %v; want a retriable error", err)
	}
	err = e.w.Handle(e.ctx, task, 3)
	if !isPermanent(err) {
		t.Fatalf("third attempt: %v; want a permanent error", err)
	}
	e.w.GiveUp(e.ctx, task, err)
	v := e.deployment(t)
	if v.Deployment.Status != domain.StatusFailed || !strings.Contains(v.Current().Error, "not found") {
		t.Fatalf("status %s, error %q", v.Deployment.Status, v.Current().Error)
	}
}

func syncedState(commit string) store.SyncState {
	return store.SyncState{SyncStatus: "Synced", HealthStatus: "Healthy", Commit: commit, Status: domain.StatusDeployed}
}

func isPermanent(err error) bool { return queue.IsPermanent(err) }

// TestBranchProtection checks that only the worker can push to the GitOps
// repositories, also a Forgejo administrator, and that the worker corrects
// a weaker rule.
func TestBranchProtection(t *testing.T) {
	e := setup(t)
	want := forgejo.BranchProtection{RuleName: "main", EnablePush: true, EnablePushWhitelist: true,
		PushWhitelistUsernames: []string{"squared-bot"}, PushWhitelistTeams: []string{},
		EnableMergeWhitelist: true, MergeWhitelistUsernames: []string{"squared-bot"}, MergeWhitelistTeams: []string{},
		ApplyToAdmins: true}
	rule := func(repo string) forgejo.BranchProtection {
		e.git.mu.Lock()
		defer e.git.mu.Unlock()
		return e.git.protections["squared/"+repo+"/main"]
	}
	// Prepare protected the platform repository.
	if got := rule("platform"); !reflect.DeepEqual(got, want) {
		t.Fatalf("platform rule = %+v", got)
	}

	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: e.tenantID(t)}, 1))
	if got := rule("tenant-acme"); !reflect.DeepEqual(got, want) {
		t.Fatalf("tenant rule = %+v", got)
	}

	// Somebody lets the administrators push. The next sync corrects it.
	e.git.mu.Lock()
	weak := want
	weak.ApplyToAdmins = false
	weak.PushWhitelistUsernames = []string{"squared-bot", "someone"}
	e.git.protections["squared/tenant-acme/main"] = weak
	e.git.mu.Unlock()
	storetest.Must(t, e.w.Handle(e.ctx, domain.Task{Kind: domain.TaskTenantSync, TenantID: e.tenantID(t)}, 1))
	if got := rule("tenant-acme"); !reflect.DeepEqual(got, want) {
		t.Fatalf("tenant rule after the correction = %+v", got)
	}
}
