package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"squared/internal/gitops"
	"squared/internal/render"
)

// argoApp is the part of an Argo CD application that decides what it
// deploys, from where, and how Argo CD keeps it in sync.
type argoApp struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
		Source  *struct {
			RepoURL        string          `json:"repoURL"`
			TargetRevision string          `json:"targetRevision"`
			Path           string          `json:"path"`
			Chart          string          `json:"chart"`
			Helm           json.RawMessage `json:"helm"`
			Kustomize      json.RawMessage `json:"kustomize"`
			Plugin         json.RawMessage `json:"plugin"`
		} `json:"source"`
		Sources     []json.RawMessage `json:"sources"`
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
func checkTenantApplication(slug string, a argoApp, r tenantProjectRules) []string {
	p := a.healProblems()
	if want := gitops.ProjectName(slug); a.Spec.Project != want {
		p = append(p, fmt.Sprintf("it uses the project %q, not %q of the tenant", a.Spec.Project, want))
	}
	if len(a.Spec.Sources) > 0 {
		p = append(p, "it has more than one source")
	}
	if s := a.Spec.Source; s == nil {
		p = append(p, "it has no source")
	} else {
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
		if s.Chart != "" || len(s.Helm) > 0 || len(s.Kustomize) > 0 || len(s.Plugin) > 0 {
			p = append(p, "its source uses Helm, Kustomize or a plugin, not the plain manifests of Squared")
		}
	}
	d := a.Spec.Destination
	if !slices.Contains(r.ClusterServers, d.Server) {
		p = append(p, fmt.Sprintf("its destination is the cluster %q, to which Squared does not deploy", cmpOr(d.Server, d.Name)))
	}
	if env, ok := strings.CutPrefix(d.Namespace, slug+"-"); !ok || !envName.MatchString(env) {
		p = append(p, fmt.Sprintf("its destination is the namespace %q, which is not a namespace of the tenant", d.Namespace))
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
	return p
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// checkArgoApplications checks the applications of Argo CD:
//   - each application that Squared made for a deployment, as
//     checkTenantApplication;
//   - an application that Squared did not make in a tenant project: only
//     Squared writes what Argo CD deploys for a tenant;
//   - the root application (rootApp), which applies the platform
//     repository with the tenant projects: it must heal itself, so that
//     a change by hand of a tenant project goes back.
func checkArgoApplications(raws []json.RawMessage, rootApp string, r tenantProjectRules) ([]string, error) {
	var problems []string
	rootSeen := false
	for _, raw := range raws {
		var a argoApp
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		l := a.Metadata.Labels
		name := a.Metadata.Name
		slug := l[render.LabelTenant]
		squared := l[render.LabelManagedBy] == render.ManagedBy && l[gitops.LabelManaged] == "true"
		switch {
		case name == rootApp:
			rootSeen = true
			for _, p := range a.healProblems() {
				problems = append(problems, fmt.Sprintf("the root application %s: %s", name, p))
			}
		case squared && slug == "":
			problems = append(problems, fmt.Sprintf("the application %s of Squared has no label %s", name, render.LabelTenant))
		case squared:
			for _, p := range checkTenantApplication(slug, a, r) {
				problems = append(problems, fmt.Sprintf("the application %s of tenant %s: %s", name, slug, p))
			}
		case strings.HasPrefix(a.Spec.Project, "tenant-"):
			problems = append(problems, fmt.Sprintf("the application %s uses the tenant project %s, but Squared did not make it", name, a.Spec.Project))
		}
	}
	if rootApp != "" && !rootSeen {
		problems = append(problems, fmt.Sprintf("the guard does not see the root application %s", rootApp))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the applications of Argo CD are not as Squared makes them: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return nil, nil
}
