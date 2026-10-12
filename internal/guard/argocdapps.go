package guard

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"squared/internal/domain"
	"squared/internal/gitops"
	"squared/internal/render"
)

// argoApp is the part of an Argo CD application that decides what it
// deploys, from where, and how Argo CD keeps it in sync.
type argoApp struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
		// The sources stay raw: the checks need the names of their fields.
		Source         json.RawMessage   `json:"source"`
		Sources        []json.RawMessage `json:"sources"`
		SourceHydrator *struct {
			DrySource struct {
				RepoURL string `json:"repoURL"`
			} `json:"drySource"`
		} `json:"sourceHydrator"`
		Destination struct {
			Server    string `json:"server"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
		SyncPolicy *struct {
			Automated *struct {
				Enabled  *bool `json:"enabled"`
				Prune    bool  `json:"prune"`
				SelfHeal bool  `json:"selfHeal"`
			} `json:"automated"`
			SyncOptions []string `json:"syncOptions"`
		} `json:"syncPolicy"`
		IgnoreDifferences []json.RawMessage `json:"ignoreDifferences"`
	} `json:"spec"`
}

// appSource is one source of an application.
type appSource struct {
	RepoURL        string                     `json:"repoURL"`
	TargetRevision string                     `json:"targetRevision"`
	Path           string                     `json:"path"`
	Directory      map[string]json.RawMessage `json:"directory"`
	// fields are the names of all fields of the source.
	fields []string
}

func parseSource(raw json.RawMessage) (appSource, error) {
	var s appSource
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	s.fields = slices.Sorted(maps.Keys(all))
	return s, nil
}

// repos returns the repository addresses of all sources of the
// application, also of the source hydrator.
func (a argoApp) repos() ([]string, error) {
	raws := a.Spec.Sources
	if len(a.Spec.Source) > 0 && string(a.Spec.Source) != "null" {
		raws = append([]json.RawMessage{a.Spec.Source}, raws...)
	}
	var out []string
	for _, raw := range raws {
		s, err := parseSource(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, s.RepoURL)
	}
	if h := a.Spec.SourceHydrator; h != nil {
		out = append(out, h.DrySource.RepoURL)
	}
	return out, nil
}

// singleSource returns the source of an application that must have only
// spec.source, and the problems if it has more.
func (a argoApp) singleSource() (*appSource, []string, error) {
	var p []string
	if len(a.Spec.Sources) > 0 {
		p = append(p, "it has more than one source")
	}
	if a.Spec.SourceHydrator != nil {
		p = append(p, "it has a source hydrator")
	}
	if len(a.Spec.Source) == 0 || string(a.Spec.Source) == "null" {
		return nil, append(p, "it has no source"), nil
	}
	s, err := parseSource(a.Spec.Source)
	if err != nil {
		return nil, nil, err
	}
	return &s, p, nil
}

// extraFields returns the problems for fields of a source that Squared
// does not set, for example helm, kustomize, plugin or chart. Argo CD
// then changes the manifests before it applies them.
func (s appSource) extraFields(allowed []string) []string {
	var p []string
	for _, f := range s.fields {
		if !slices.Contains(allowed, f) {
			p = append(p, fmt.Sprintf("its source has the field %s, which Squared does not set", f))
		}
	}
	return p
}

// repoKey returns an address of a repository in the form that Argo CD
// compares: without user, without ".git" and "/" at the end, and in lower
// case. Forgejo also ignores the case of owner and repository names.
func repoKey(repo string) string {
	u, err := url.Parse(strings.TrimSpace(repo))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git"))
	}
	p := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	return strings.ToLower(u.Scheme + "://" + u.Host + p)
}

// isGitOpsRepo tells if a repository address is in one of the prefixes.
func isGitOpsRepo(repo string, prefixes []string) bool {
	key := repoKey(repo) + "/"
	for _, prefix := range prefixes {
		if strings.HasPrefix(key, strings.TrimSuffix(repoKey(prefix), "/")+"/") {
			return true
		}
	}
	return false
}

// squaredSyncOptions are the sync options that Squared sets on the
// application of a deployment (gitops.Config.ApplicationFile).
var squaredSyncOptions = []string{"CreateNamespace=false", "PruneLast=true"}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// healProblems returns the problems of a sync policy: Argo CD must sync
// automatically, remove objects that are not in Git, and change back
// changes by hand in the cluster (decision D1).
func (a argoApp) healProblems() []string {
	sp := a.Spec.SyncPolicy
	switch {
	case sp == nil || sp.Automated == nil || (sp.Automated.Enabled != nil && !*sp.Automated.Enabled):
		return []string{"it does not sync automatically, thus Git and the cluster can differ"}
	case !sp.Automated.SelfHeal:
		return []string{"selfHeal is off, thus changes by hand in the cluster stay"}
	case !sp.Automated.Prune:
		return []string{"prune is off, thus objects that are not in Git stay"}
	}
	return nil
}

// checkTenantApplication compares an application that Squared made for a
// deployment with what Squared puts into it.
func checkTenantApplication(slug string, a argoApp, r tenantProjectRules) ([]string, error) {
	p := a.healProblems()
	if want := gitops.ProjectName(slug); a.Spec.Project != want {
		p = append(p, fmt.Sprintf("it uses the project %q, not %q of the tenant", a.Spec.Project, want))
	}
	env, name := a.Metadata.Labels[gitops.LabelEnvironment], a.Metadata.Labels[render.LabelDeployment]
	if !envName.MatchString(env) || name == "" || a.Metadata.Name != domain.AppName(slug, env, name) {
		p = append(p, fmt.Sprintf("its labels %s=%q and %s=%q do not agree with its name", gitops.LabelEnvironment, env, render.LabelDeployment, name))
	}

	s, sp, err := a.singleSource()
	if err != nil {
		return nil, err
	}
	p = append(p, sp...)
	if s != nil {
		var want []string
		for _, prefix := range r.RepoPrefixes {
			want = append(want, strings.TrimSuffix(prefix, "/")+"/"+gitops.TenantRepo(slug)+".git")
		}
		if !slices.Contains(want, s.RepoURL) {
			p = append(p, fmt.Sprintf("its source %q is not the repository of the tenant", s.RepoURL))
		}
		if !commitSHA.MatchString(s.TargetRevision) {
			p = append(p, fmt.Sprintf("it follows %q, not a fixed commit", s.TargetRevision))
		}
		// The directory of another deployment or environment has other
		// manifests, also for another namespace.
		if want := gitops.DeploymentDir(env, name); s.Path != want {
			p = append(p, fmt.Sprintf("its source path is %q, not the directory %q of the deployment", s.Path, want))
		}
		p = append(p, s.extraFields([]string{"repoURL", "targetRevision", "path"})...)
	}

	d := a.Spec.Destination
	if !slices.Contains(r.ClusterServers, d.Server) {
		p = append(p, fmt.Sprintf("its destination is the cluster %q, to which Squared does not deploy", cmp.Or(d.Server, d.Name)))
	}
	if d.Namespace != domain.Namespace(slug, env) {
		p = append(p, fmt.Sprintf("its destination is the namespace %q, not the namespace of its environment", d.Namespace))
	}
	if sp := a.Spec.SyncPolicy; sp != nil {
		for _, o := range sp.SyncOptions {
			if !slices.Contains(squaredSyncOptions, o) {
				p = append(p, fmt.Sprintf("it has the sync option %q, which Squared does not set", o))
			}
		}
	}
	if len(a.Spec.IgnoreDifferences) > 0 {
		p = append(p, "it ignores differences, thus selfHeal does not change them back")
	}
	return p, nil
}

// RootSource is the source of the root application: the platform
// repository, its branch and the directory with the tenants.
type RootSource struct {
	RepoURL        string `json:"repoURL"`
	TargetRevision string `json:"targetRevision"`
	Path           string `json:"path"`
}

// checkRootApplication checks the root application. Its source must be
// the platform repository, which only the worker writes. A filter of the
// directory can hide the tenant projects from it.
func checkRootApplication(a argoApp, root RootSource) ([]string, error) {
	p := a.healProblems()
	s, sp, err := a.singleSource()
	if err != nil {
		return nil, err
	}
	p = append(p, sp...)
	if s == nil {
		return p, nil
	}
	if s.RepoURL != root.RepoURL {
		p = append(p, fmt.Sprintf("its source %q is not the platform repository", s.RepoURL))
	}
	if s.TargetRevision != root.TargetRevision {
		p = append(p, fmt.Sprintf("it follows %q, not %q", s.TargetRevision, root.TargetRevision))
	}
	if path.Clean(s.Path) != path.Clean(root.Path) {
		p = append(p, fmt.Sprintf("its source path is %q, not %q", s.Path, root.Path))
	}
	p = append(p, s.extraFields([]string{"repoURL", "targetRevision", "path", "directory"})...)
	for _, f := range slices.Sorted(maps.Keys(s.Directory)) {
		if f != "recurse" {
			p = append(p, fmt.Sprintf("its source has the field directory.%s, which Squared does not set", f))
		}
	}
	return p, nil
}

// argoAppRules are what the applications of Argo CD may have.
type argoAppRules struct {
	tenantProjectRules
	Namespace string     // The namespace of Argo CD.
	RootApp   string     // The name of the root application.
	Root      RootSource // The source of the root application.
}

// checkArgoApplications checks the applications of Argo CD in all
// namespaces:
//   - each application that Squared made for a deployment, as
//     checkTenantApplication. Squared makes them only in the namespace of
//     Argo CD;
//   - an application that Squared did not make in a tenant project, or
//     with a source in the GitOps repositories: only Squared writes what
//     Argo CD deploys from them, and only with the limits of the tenant
//     project;
//   - the root application (RootApp), which applies the platform
//     repository with the tenant projects, as checkRootApplication. It
//     must heal itself, so that a change by hand of a tenant project goes
//     back.
func checkArgoApplications(raws []json.RawMessage, r argoAppRules) ([]string, error) {
	var problems []string
	rootSeen := false
	for _, raw := range raws {
		var a argoApp
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		l := a.Metadata.Labels
		name := a.Metadata.Name
		if a.Metadata.Namespace != r.Namespace {
			name = a.Metadata.Namespace + "/" + name
		}
		slug := l[render.LabelTenant]
		squared := l[render.LabelManagedBy] == render.ManagedBy && l[gitops.LabelManaged] == "true"
		var p []string
		var err error
		switch {
		case name == r.RootApp:
			rootSeen = true
			p, err = checkRootApplication(a, r.Root)
			for i := range p {
				p[i] = fmt.Sprintf("the root application %s: %s", name, p[i])
			}
		case squared && a.Metadata.Namespace != r.Namespace:
			p = []string{fmt.Sprintf("the application %s of Squared is not in the namespace %s", name, r.Namespace)}
		case squared && slug == "":
			p = []string{fmt.Sprintf("the application %s of Squared has no label %s", name, render.LabelTenant)}
		case squared:
			p, err = checkTenantApplication(slug, a, r.tenantProjectRules)
			for i := range p {
				p[i] = fmt.Sprintf("the application %s of tenant %s: %s", name, slug, p[i])
			}
		default:
			if strings.HasPrefix(a.Spec.Project, "tenant-") {
				p = append(p, fmt.Sprintf("the application %s uses the tenant project %s, but Squared did not make it", name, a.Spec.Project))
			}
			var repos []string
			repos, err = a.repos()
			for _, repo := range repos {
				if isGitOpsRepo(repo, r.RepoPrefixes) {
					p = append(p, fmt.Sprintf("the application %s uses the GitOps repository %s, but Squared did not make it", name, repo))
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("application %s: %w", name, err)
		}
		problems = append(problems, p...)
	}
	if !rootSeen {
		problems = append(problems, fmt.Sprintf("the guard does not see the root application %s", r.RootApp))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the applications of Argo CD are not as Squared makes them: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return nil, nil
}
