// Package guard protects the identity provider groups that give rights in
// the platform services.
//
// The Squared worker may manage Keycloak groups and their members, because
// it copies the members of tenants to tenant groups. Keycloak cannot limit
// that right to the groups that the worker made. Thus the guard reads the
// configuration of each service, finds every group that the configuration
// gives rights to, and puts these groups in the Keycloak permission
// "protected-groups". That permission replaces the group permission of the
// worker for these groups, thus the worker can neither see nor change
// them.
//
// The guard is a separate process with its own credentials. The worker
// cannot reach it or change its configuration.
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"squared/internal/keycloak"
	"squared/internal/metrics"
)

// Reference is one place in a service configuration that gives rights to
// a group.
type Reference struct {
	Group   string `json:"group"`   // Full path, as in the groups claim, for example "/squared-admins".
	Service string `json:"service"` // For example "Argo CD".
	Where   string `json:"where"`   // For example "argocd/argocd-rbac-cm policy.csv".
}

// Source reads the groups that one service configuration gives rights to.
type Source interface {
	Name() string
	References(ctx context.Context) ([]Reference, error)
}

// Config configures the guard.
type Config struct {
	// Permission is the name of the Keycloak scope permission that holds
	// the protected groups.
	Permission string
	// Groups are protected in all cases, for example the administrator
	// group of Squared.
	Groups []string
}

// Guard keeps the protected groups up to date.
type Guard struct {
	cfg     Config
	sources []Source
	kc      *keycloak.Client
	log     *slog.Logger

	runs     *metrics.Counter
	changes  *metrics.Counter
	warnings *metrics.Counter

	mu     sync.Mutex
	status Status
}

// Status is the result of the last run.
type Status struct {
	Time      time.Time        `json:"time"`
	Protected []ProtectedGroup `json:"protected"`
	Errors    []string         `json:"errors,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
	Changed   bool             `json:"changed"`
}

// ProtectedGroup is one protected group and the places that refer to it.
type ProtectedGroup struct {
	Path       string      `json:"path"`
	ID         string      `json:"id"`
	References []Reference `json:"references"`
}

// New returns a Guard. reg can be nil.
func New(cfg Config, sources []Source, kc *keycloak.Client, log *slog.Logger, reg *metrics.Registry) *Guard {
	if cfg.Permission == "" {
		cfg.Permission = "protected-groups"
	}
	if reg == nil {
		reg = &metrics.Registry{}
	}
	return &Guard{
		cfg: cfg, sources: sources, kc: kc, log: log,
		runs:     reg.NewCounter("squared_guard_runs_total", "Runs of the guard by result (ok, error).", "result"),
		changes:  reg.NewCounter("squared_guard_changes_total", "Changes of the protected groups.", "change"),
		warnings: reg.NewCounter("squared_guard_warnings_total", "Warnings, for example a new protected group that has members.", "kind"),
	}
}

// Run protects the groups at an interval until ctx is done.
func (g *Guard) Run(ctx context.Context, interval time.Duration) {
	for {
		start := time.Now()
		st, err := g.Once(ctx)
		if err != nil && ctx.Err() == nil {
			g.runs.Inc("error")
			g.log.Error("guard run failed", "error", err)
		} else if err == nil {
			g.runs.Inc("ok")
			g.log.Debug("guard run", "protected", len(st.Protected), "duration_ms", time.Since(start).Milliseconds())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Once reads all sources and updates the permission one time.
func (g *Guard) Once(ctx context.Context) (Status, error) {
	st := Status{Time: time.Now().UTC()}
	refs := make([]Reference, 0, len(g.cfg.Groups))
	for _, group := range g.cfg.Groups {
		refs = append(refs, Reference{Group: group, Service: "guard", Where: "configuration of the guard"})
	}
	sourceFailed := false
	for _, s := range g.sources {
		r, err := s.References(ctx)
		if err != nil {
			// Without the references of this source, the guard cannot
			// tell which groups to remove. It only adds groups in this run.
			sourceFailed = true
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", s.Name(), err))
			g.log.Error("guard cannot read a source; no group is removed in this run", "source", s.Name(), "error", err)
			continue
		}
		refs = append(refs, r...)
	}

	byGroup := map[string][]Reference{}
	for _, r := range refs {
		if slices.Contains(byGroup[r.Group], r) {
			continue // For example the "add" and the "update" command of the Forgejo script.
		}
		if !strings.HasPrefix(r.Group, "/") {
			msg := fmt.Sprintf("%s (%s) refers to %q, which is not a full group path; it matches no group", r.Service, r.Where, r.Group)
			st.Warnings = append(st.Warnings, msg)
			g.warnings.Inc("short-name")
			g.log.Warn("guard: " + msg)
			continue
		}
		byGroup[r.Group] = append(byGroup[r.Group], r)
	}

	perm, err := g.kc.GetPermission(ctx, g.cfg.Permission)
	if err != nil {
		return g.finish(st), err
	}
	current := map[string]bool{}
	for _, id := range perm.Resources {
		current[id] = true
	}

	paths := make([]string, 0, len(byGroup))
	for p := range byGroup {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var ids []string
	for _, path := range paths {
		group, err := g.resolve(ctx, path)
		if err != nil {
			sourceFailed = true
			st.Errors = append(st.Errors, err.Error())
			g.log.Error("guard cannot protect a group", "group", path, "error", err)
			continue
		}
		ids = append(ids, group.ID)
		st.Protected = append(st.Protected, ProtectedGroup{Path: path, ID: group.ID, References: byGroup[path]})
		if !current[group.ID] {
			g.warnIfMembers(ctx, &st, path, group.ID, byGroup[path])
		}
	}
	if sourceFailed {
		for id := range current {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	old := append([]string(nil), perm.Resources...)
	sort.Strings(old)
	if slices.Equal(ids, old) {
		return g.finish(st), nil
	}
	if err := g.kc.SetPermissionResources(ctx, perm, ids); err != nil {
		return g.finish(st), fmt.Errorf("update permission %s: %w", g.cfg.Permission, err)
	}
	st.Changed = true
	for _, id := range ids {
		if !current[id] {
			g.changes.Inc("protected")
			g.log.Info("guard: group is protected now", "id", id, "groups", pathOf(st.Protected, id))
		}
	}
	for _, id := range old {
		if !slices.Contains(ids, id) {
			g.changes.Inc("released")
			g.log.Info("guard: group is not protected anymore; no configuration refers to it", "id", id)
		}
	}
	return g.finish(st), nil
}

func pathOf(groups []ProtectedGroup, id string) string {
	for _, p := range groups {
		if p.ID == id {
			return p.Path
		}
	}
	return ""
}

// resolve returns the Keycloak group of a path. It makes a missing
// top-level group at once: otherwise the worker could make the group and
// add members before a configuration change takes effect.
func (g *Guard) resolve(ctx context.Context, path string) (*keycloak.Group, error) {
	group, err := g.kc.GroupByPath(ctx, path)
	if err != nil || group != nil {
		return group, err
	}
	name := strings.TrimPrefix(path, "/")
	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("group %s does not exist; the guard makes only top-level groups", path)
	}
	group, err = g.kc.EnsureGroup(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("make group %s: %w", path, err)
	}
	g.log.Info("guard: made a missing group, so that it is protected before somebody adds members", "group", path)
	return group, nil
}

// warnIfMembers warns about a group that becomes protected and has
// members. Somebody could have added the members before a configuration
// gave rights to the group.
func (g *Guard) warnIfMembers(ctx context.Context, st *Status, path, id string, refs []Reference) {
	members, err := g.kc.Members(ctx, id)
	if err != nil || len(members) == 0 {
		return
	}
	msg := fmt.Sprintf("group %s becomes protected and has %d members; check that they must have the rights from %s", path, len(members), refs[0].Service)
	st.Warnings = append(st.Warnings, msg)
	g.warnings.Inc("members")
	g.log.Warn("guard: "+msg, "where", refs[0].Where)
}

func (g *Guard) finish(st Status) Status {
	g.mu.Lock()
	g.status = st
	g.mu.Unlock()
	return st
}

// StatusHandler serves the status of the last run as JSON.
func (g *Guard) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		st := g.status
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
	})
}
