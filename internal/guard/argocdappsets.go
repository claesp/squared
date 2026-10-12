package guard

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"squared/internal/gitops"
	"squared/internal/render"
)

// argoAppSet is the part of an Argo CD ApplicationSet that decides which
// applications it makes.
type argoAppSet struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Generators    []json.RawMessage `json:"generators"`
		Template      json.RawMessage   `json:"template"`
		TemplatePatch string            `json:"templatePatch"`
		SyncPolicy    *struct {
			ApplicationsSync            string `json:"applicationsSync"`
			PreserveResourcesOnDeletion bool   `json:"preserveResourcesOnDeletion"`
		} `json:"syncPolicy"`
	} `json:"spec"`
}

// templated tells if a value comes from the parameters of the generators.
func templated(s string) bool { return strings.Contains(s, "{{") }

// checkAppTemplate returns the problems of an application template: of
// the ApplicationSet, or of a generator, which changes some fields of it.
// The applications must not look like the applications of Squared, must
// not use a tenant project, and must not use the GitOps repositories.
// The guard cannot know the value of a field that comes from the
// parameters of the generators, thus such a project or repository is a
// problem too.
//
// An application with the name of an application of Squared (names) takes
// that application over (see argoApp.ownerProblems).
func checkAppTemplate(raw json.RawMessage, prefixes []string, names map[string]bool) ([]string, error) {
	var a argoApp
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	var p []string
	if n := a.Metadata.Name; names[n] {
		p = append(p, fmt.Sprintf("it makes the application %s, thus it takes over the application of Squared with that name", n))
	}
	for _, k := range []string{render.LabelManagedBy, gitops.LabelManaged, render.LabelTenant} {
		if _, ok := a.Metadata.Labels[k]; ok {
			p = append(p, fmt.Sprintf("its applications get the label %s, which only Squared sets", k))
		}
	}
	switch project := a.Spec.Project; {
	case templated(project):
		p = append(p, fmt.Sprintf("the project %q of its applications comes from the generators, thus it can be a tenant project", project))
	case strings.HasPrefix(project, "tenant-"):
		p = append(p, fmt.Sprintf("its applications use the tenant project %s", project))
	}
	repos, err := a.repos()
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		switch {
		case templated(repo):
			p = append(p, fmt.Sprintf("the repository %q of its applications comes from the generators, thus it can be a GitOps repository", repo))
		case isGitOpsRepo(repo, prefixes):
			p = append(p, fmt.Sprintf("its applications use the GitOps repository %s", repo))
		}
	}
	return p, nil
}

// scmCredentialFields are the fields of the SCM provider and pull request
// generators that name credentials.
var scmCredentialFields = []string{"tokenRef", "passwordRef", "basicAuth", "bearerToken", "appSecretName", "accessTokenRef"}

// checkGenerator returns the problems of a generator and of the
// generators in it (matrix, merge). A generator that reads a GitOps
// repository, through Git or through the API of Forgejo, gives
// parameters from it to the applications. A generator can also have its
// own template.
func checkGenerator(v any, prefixes []string, names map[string]bool) ([]string, error) {
	var p []string
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			ep, err := checkGenerator(e, prefixes, names)
			if err != nil {
				return nil, err
			}
			p = append(p, ep...)
		}
	case map[string]any:
		if repo, ok := v["repoURL"].(string); ok {
			switch {
			case templated(repo):
				p = append(p, fmt.Sprintf("a generator reads the repository %q, which comes from other generators", repo))
			case isGitOpsRepo(repo, prefixes):
				p = append(p, fmt.Sprintf("a generator reads the GitOps repository %s", repo))
			}
		}
		// The SCM provider and pull request generators of Gitea and
		// Forgejo: the address of the API is the address of the server.
		api, _ := v["api"].(string)
		owner, _ := v["owner"].(string)
		if api != "" && owner != "" {
			repo, _ := v["repo"].(string)
			addr := strings.TrimSuffix(api, "/") + "/" + owner + "/" + cmp.Or(repo, "any")
			if isGitOpsRepo(addr, prefixes) {
				p = append(p, fmt.Sprintf("a generator reads the GitOps repositories of %s at %s", owner, api))
			}
		}
		// A provider with credentials sends them to its API.
		if slices.ContainsFunc(scmCredentialFields, func(f string) bool { _, ok := v[f]; return ok }) {
			if insecure, _ := v["insecure"].(bool); insecure {
				p = append(p, fmt.Sprintf("a generator sends credentials to %s without the check of the TLS certificate (insecure)", cmp.Or(api, "its server")))
			}
			if strings.HasPrefix(strings.ToLower(api), "http://") {
				p = append(p, fmt.Sprintf("a generator sends credentials to %s without TLS", api))
			}
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if t, ok := v[k].(map[string]any); ok && k == "template" {
				raw, err := json.Marshal(t)
				if err != nil {
					return nil, err
				}
				tp, err := checkAppTemplate(raw, prefixes, names)
				if err != nil {
					// For example a parameter with the name "template" of a
					// list generator.
					tp = []string{fmt.Sprintf("the guard cannot read it (%v)", err)}
				}
				for _, m := range tp {
					p = append(p, "in the template of a generator, "+m)
				}
				continue
			}
			kp, err := checkGenerator(v[k], prefixes, names)
			if err != nil {
				return nil, err
			}
			p = append(p, kp...)
		}
	}
	return p, nil
}

// checkArgoAppSets checks the ApplicationSets of Argo CD in all
// namespaces. Squared makes none. The guard also checks the applications
// that they make, but an ApplicationSet makes them again after a removal.
// names are the applications of Squared in the namespace of Argo CD.
//
// An ApplicationSet must have preserveResourcesOnDeletion: it can take
// over an application of Squared with the same name, also with the policy
// create-only. Without the setting, the application keeps the finalizer
// of Argo CD, and the removal of the ApplicationSet, or of the
// application by the policy, removes the objects of the tenant too. With
// it, only the application goes, and the root application makes it again.
func checkArgoAppSets(raws []json.RawMessage, r argoAppRules, names map[string]bool) ([]string, error) {
	var problems []string
	for _, raw := range raws {
		var s argoAppSet
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		name := s.Metadata.Name
		if s.Metadata.Namespace != r.Namespace {
			name = s.Metadata.Namespace + "/" + name
		}
		// It makes applications in its own namespace.
		own := names
		if s.Metadata.Namespace != r.Namespace {
			own = nil
		}
		var p []string
		if s.Spec.SyncPolicy == nil || !s.Spec.SyncPolicy.PreserveResourcesOnDeletion {
			p = append(p, "preserveResourcesOnDeletion is not true: when it takes over an application of Squared with the same name, also with the policy create-only, its removal removes the objects of the tenant")
		}
		if len(s.Spec.Template) > 0 {
			tp, err := checkAppTemplate(s.Spec.Template, r.RepoPrefixes, own)
			if err != nil {
				return nil, fmt.Errorf("ApplicationSet %s: %w", name, err)
			}
			p = append(p, tp...)
		}
		if strings.TrimSpace(s.Spec.TemplatePatch) != "" {
			p = append(p, "it has a templatePatch, which can change the project and the sources of its applications")
		}
		for _, g := range s.Spec.Generators {
			var v any
			if err := json.Unmarshal(g, &v); err != nil {
				return nil, fmt.Errorf("ApplicationSet %s: %w", name, err)
			}
			gp, err := checkGenerator(v, r.RepoPrefixes, own)
			if err != nil {
				return nil, fmt.Errorf("ApplicationSet %s: %w", name, err)
			}
			p = append(p, gp...)
		}
		for _, m := range p {
			problems = append(problems, fmt.Sprintf("the ApplicationSet %s: %s", name, m))
		}
	}
	return problems, nil
}
