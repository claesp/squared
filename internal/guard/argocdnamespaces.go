package guard

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"squared/internal/kube"
)

// argoParam is a parameter of an Argo CD component.
type argoParam struct {
	Key  string // The key in argocd-cmd-params-cm.
	Env  string // The environment variable that the key goes into.
	Flag string // The command line flag, which is stronger than the variable.
}

// argoNamespaceSettings are the parameters with the namespaces, other than
// its own, in which Argo CD reads applications or ApplicationSets. Each
// value is a list of names, glob patterns such as "team-*", or regular
// expressions between "/".
var argoNamespaceSettings = []argoParam{
	{"application.namespaces", "ARGOCD_APPLICATION_NAMESPACES", "--application-namespaces"},
	{"applicationsetcontroller.namespaces", "ARGOCD_APPLICATIONSET_CONTROLLER_NAMESPACES", "--applicationset-namespaces"},
}

// argoParamsConfigMap is the ConfigMap with the parameters of the Argo CD
// components.
const argoParamsConfigMap = "argocd-cmd-params-cm"

// argoWorkload is a component of Argo CD that reads one of the settings.
type argoWorkload struct {
	Resource, Name string
}

// argoNamespaceWorkloads are the components of Argo CD that read the
// settings.
var argoNamespaceWorkloads = []argoWorkload{
	{"statefulsets", "argocd-application-controller"},
	{"deployments", "argocd-server"},
	{"deployments", argoAppSetController},
}

// namespaceValue is one value of a setting and where the guard found it.
type namespaceValue struct {
	Setting, Where, Value string
}

// argoNamespaceValues returns the values of the namespace settings: from
// the ConfigMap, and from the flags and the environment variables of the
// containers. A value in the ConfigMap counts also when no container reads
// it. It returns problems for values that the guard cannot read.
func argoNamespaceValues(params map[string]string, containers map[string][]kube.Container) ([]namespaceValue, []string) {
	var values []namespaceValue
	var problems []string
	for _, s := range argoNamespaceSettings {
		if v := params[s.Key]; strings.TrimSpace(v) != "" {
			values = append(values, namespaceValue{s.Key, argoParamsConfigMap, v})
		}
	}
	for _, w := range slices.Sorted(maps.Keys(containers)) {
		for _, c := range containers[w] {
			where := fmt.Sprintf("%s container %s", w, c.Name)
			if len(c.EnvFrom) > 0 {
				problems = append(problems, fmt.Sprintf("%s reads environment variables with envFrom, which the guard does not read", where))
			}
			for _, s := range argoNamespaceSettings {
				for _, e := range c.Env {
					if e.Name != s.Env {
						continue
					}
					switch {
					case e.ValueFrom == nil:
						values = append(values, namespaceValue{s.Key, where + " " + s.Env, e.Value})
					case e.ValueFrom.ConfigMapKeyRef != nil &&
						e.ValueFrom.ConfigMapKeyRef.Name == argoParamsConfigMap && e.ValueFrom.ConfigMapKeyRef.Key == s.Key:
						// The value of the ConfigMap is in values already.
					default:
						problems = append(problems, fmt.Sprintf("%s reads %s from another place than the key %s of %s", where, s.Env, s.Key, argoParamsConfigMap))
					}
				}
				args := append(slices.Clone(c.Command), c.Args...)
				for i, a := range args {
					if v, ok := strings.CutPrefix(a, s.Flag+"="); ok {
						values = append(values, namespaceValue{s.Key, where + " " + s.Flag, v})
					} else if a == s.Flag && i+1 < len(args) {
						values = append(values, namespaceValue{s.Key, where + " " + s.Flag, args[i+1]})
					}
				}
			}
		}
	}
	return values, problems
}

// checkArgoNamespaces returns the problems for namespaces in which Argo CD
// reads applications or ApplicationSets. Squared makes them only in the
// namespace of Argo CD (own). In another namespace, a person who can make
// applications there deploys with each project that lists the namespace
// in sourceNamespaces, and an ApplicationSet there makes such
// applications. allowed are the values that an operator checked.
func checkArgoNamespaces(values []namespaceValue, own string, allowed []string) []string {
	var p []string
	for _, v := range values {
		for _, ns := range strings.Split(v.Value, ",") {
			ns = strings.TrimSpace(ns)
			if ns == "" || ns == own || slices.Contains(allowed, ns) {
				continue
			}
			p = append(p, fmt.Sprintf("%s (%s) has the namespace %q, which is not in applicationNamespaces", v.Setting, v.Where, ns))
		}
	}
	return p
}
