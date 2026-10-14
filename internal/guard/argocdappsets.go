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
func templated(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, patchParameter)
}

// resourcesFinalizer is the finalizer of Argo CD that removes the objects of
// an application with it, also with the suffixes /background and
// /foreground.
const resourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

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
	for _, f := range a.Metadata.Finalizers {
		if f == resourcesFinalizer || strings.HasPrefix(f, resourcesFinalizer+"/") {
			p = append(p, fmt.Sprintf("its applications get the finalizer %s: their removal removes their objects, also with preserveResourcesOnDeletion", f))
		}
	}
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

// generatorKinds are the generators of Argo CD 3.5.
var generatorKinds = []string{"clusterDecisionResource", "clusters", "git", "list", "matrix", "merge", "plugin", "pullRequest", "scmProvider"}

// checkGenerators returns the problems and the warnings of a list of
// generators: of an ApplicationSet, or in a matrix or merge generator, and
// the ConfigMaps of the plugin generators.
// Each generator has one kind, an optional selector, and in the kind an
// optional template, which changes fields of the template of the
// ApplicationSet for the applications of this generator.
func checkGenerators(gens []any, prefixes []string, names map[string]bool) (problems, warnings, plugins []string) {
	for _, g := range gens {
		m, ok := g.(map[string]any)
		if !ok {
			problems = append(problems, "the guard cannot read a generator, which is not an object")
			continue
		}
		for _, kind := range slices.Sorted(maps.Keys(m)) {
			if kind == "selector" {
				continue
			}
			if !slices.Contains(generatorKinds, kind) {
				warnings = append(warnings, fmt.Sprintf("a generator has the kind %s, which the guard does not know: it checks only the applications that it makes", kind))
				continue
			}
			body, ok := m[kind].(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("the guard cannot read a %s generator, which is not an object", kind))
				continue
			}
			reads := map[string]any{}
			for k, v := range body {
				switch {
				case k == "template":
					problems = append(problems, checkGeneratorTemplate(kind, v, prefixes, names)...)
				case k == "generators" && (kind == "matrix" || kind == "merge"):
					children, ok := v.([]any)
					if !ok {
						problems = append(problems, fmt.Sprintf("the guard cannot read the generators of a %s generator", kind))
						continue
					}
					cp, cw, cpl := checkGenerators(children, prefixes, names)
					problems = append(problems, cp...)
					warnings = append(warnings, cw...)
					plugins = append(plugins, cpl...)
				case k == "configMapRef" && kind == "plugin":
					ref, _ := v.(map[string]any)
					switch name, _ := ref["name"].(string); {
					case name == "":
						problems = append(problems, "the guard cannot read the ConfigMap of a plugin generator")
					case templated(name):
						problems = append(problems, fmt.Sprintf("the ConfigMap %q of a plugin generator comes from other generators", name))
					default:
						plugins = append(plugins, name)
					}
				case k == "values" || kind == "list" && (k == "elements" || k == "elementsYaml") || kind == "plugin" && k == "input":
					// Parameters, not addresses that the generator reads.
					// The checks of the templates find a project or a
					// repository from the parameters.
				default:
					reads[k] = v
				}
			}
			problems = append(problems, checkGeneratorReads(reads, prefixes)...)
		}
	}
	return problems, warnings, plugins
}

// checkGeneratorTemplate returns the problems of the template of a
// generator. It has the same checks as the template of the ApplicationSet.
func checkGeneratorTemplate(kind string, v any, prefixes []string, names map[string]bool) []string {
	prefix := fmt.Sprintf("in the template of a %s generator, ", kind)
	t, ok := v.(map[string]any)
	if !ok {
		return []string{prefix + "the guard cannot read it, because it is not an object"}
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return []string{fmt.Sprintf("%sthe guard cannot read it (%s)", prefix, strings.ReplaceAll(err.Error(), ";", ","))}
	}
	p, err := checkAppTemplate(raw, prefixes, names)
	if err != nil {
		return []string{fmt.Sprintf("%sthe guard cannot read it (%s)", prefix, strings.ReplaceAll(err.Error(), ";", ","))}
	}
	for i := range p {
		p[i] = prefix + p[i]
	}
	return p
}

// checkGeneratorReads returns the problems of what a generator reads,
// and of the credentials that it sends. A generator that reads a GitOps
// repository, through Git or through the API of Forgejo, gives parameters
// from it to the applications.
func checkGeneratorReads(v any, prefixes []string) []string {
	var p []string
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			p = append(p, checkGeneratorReads(e, prefixes)...)
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
			p = append(p, checkGeneratorReads(v[k], prefixes)...)
		}
	}
	return p
}

// checkArgoAppSets checks the ApplicationSets of Argo CD in all
// namespaces. Squared makes none. The guard also checks the applications
// that they make, but an ApplicationSet makes them again after a removal.
// names are the applications of Squared in the namespace of Argo CD, and
// policy is the policy of the ApplicationSet controller.
//
// An ApplicationSet must have preserveResourcesOnDeletion: it can take
// over an application of Squared with the same name, also with the policy
// create-only. Without the setting, the application keeps the finalizer
// of Argo CD, and the removal of the ApplicationSet, or of the
// application by the policy, removes the objects of the tenant too. With
// it, only the application goes, and the root application makes it again.
//
// It also returns the ConfigMaps of the plugin generators, with the
// ApplicationSets that use them.
func checkArgoAppSets(raws []json.RawMessage, r argoAppRules, names map[string]bool, policy argoSetPolicy) (problems, warnings []string, plugins map[string][]string, err error) {
	plugins = map[string][]string{}
	for _, raw := range raws {
		var s argoAppSet
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, nil, err
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
		var ownPolicy string
		if s.Spec.SyncPolicy == nil || !s.Spec.SyncPolicy.PreserveResourcesOnDeletion {
			p = append(p, "preserveResourcesOnDeletion is not true: when it takes over an application of Squared with the same name, also with the policy create-only, its removal removes the objects of the tenant")
		}
		if s.Spec.SyncPolicy != nil {
			ownPolicy = s.Spec.SyncPolicy.ApplicationsSync
		}
		p = append(p, appSetPolicyProblems(policy, ownPolicy)...)
		if len(s.Spec.Template) > 0 {
			tp, err := checkAppTemplate(s.Spec.Template, r.RepoPrefixes, own)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("ApplicationSet %s: %w", name, err)
			}
			p = append(p, tp...)
		}
		if strings.TrimSpace(s.Spec.TemplatePatch) != "" {
			pp, err := checkTemplatePatch(s.Spec.TemplatePatch, r.RepoPrefixes, own)
			if err != nil {
				pp = []string{fmt.Sprintf("the guard cannot read its templatePatch (%s), which can change all fields of its applications", strings.ReplaceAll(err.Error(), ";", ","))}
			}
			p = append(p, pp...)
		}
		var gens []any
		for _, g := range s.Spec.Generators {
			var v any
			if err := json.Unmarshal(g, &v); err != nil {
				return nil, nil, nil, fmt.Errorf("ApplicationSet %s: %w", name, err)
			}
			gens = append(gens, v)
		}
		gp, gw, gpl := checkGenerators(gens, r.RepoPrefixes, own)
		p = append(p, gp...)
		for _, cm := range gpl {
			if !slices.Contains(plugins[cm], name) {
				plugins[cm] = append(plugins[cm], name)
			}
		}
		for _, m := range p {
			problems = append(problems, fmt.Sprintf("the ApplicationSet %s: %s", name, m))
		}
		for _, m := range gw {
			warnings = append(warnings, fmt.Sprintf("the ApplicationSet %s: %s", name, m))
		}
	}
	return problems, warnings, plugins, nil
}
