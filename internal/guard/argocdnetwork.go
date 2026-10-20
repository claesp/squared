package guard

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"squared/internal/kube"
)

// protectedPort is a port of a component of Argo CD that does not check
// who connects, thus only some components may connect to it.
type protectedPort struct {
	Workload string   // The Deployment, or the StatefulSet with Resource "statefulsets".
	Resource string   // "" for deployments.
	Port     string   // The name of the container port.
	Callers  []string // The components that may connect: app.kubernetes.io/name.
	// Namespaces are the namespaces whose pods may connect
	// (kubernetes.io/metadata.name), for example of Prometheus.
	Namespaces []string
	Why        string // For the messages.
	// Optional tells that the component can run without the port.
	Optional bool
}

// resource returns the resource of the workload.
func (pp protectedPort) resource() string { return cmp.Or(pp.Resource, "deployments") }

// metricsWorkloads are the components of Argo CD with a port metrics, and
// the resources of their workloads.
var metricsWorkloads = []struct{ Workload, Resource string }{
	{argoAppController, "statefulsets"}, {argoServer, ""}, {argoRepoServer, ""}, {argoAppSetController, ""},
	{argoRedis, ""}, {argoDex, ""}, {argoNotifications, ""}, {argoCommitServer, ""},
}

// metricsPorts returns the protected ports for the metrics of the
// components: only pods in the namespaces (metricsNamespaces) may connect.
// The metrics of the application controller tell the name, the project,
// the repository and the destination of each application, thus of all
// tenants, and those of the repository server the repositories.
func metricsPorts(metricsNamespaces []string) []protectedPort {
	var p []protectedPort
	for _, w := range metricsWorkloads {
		p = append(p, protectedPort{Workload: w.Workload, Resource: w.Resource, Port: "metrics", Namespaces: metricsNamespaces, Optional: true,
			Why: "the metrics tell the applications, the repositories and the namespaces of all tenants"})
	}
	return p
}

var protectedPorts = []protectedPort{
	{Workload: argoRepoServer, Port: "repo-server", Callers: []string{argoServer, argoAppController, argoAppSetController, argoNotifications},
		Why: "it renders the manifests of each repository with the credentials of Argo CD for each caller"},
	{Workload: argoRedis, Port: "redis", Callers: []string{argoServer, argoRepoServer, argoAppController},
		Why: "it has the cache with the manifests, which the application controller trusts"},
	{Workload: argoCommitServer, Port: "server", Callers: []string{argoAppController},
		Why: "it pushes to Git with the write credentials of the repositories"},
	{Workload: argoDex, Port: "http", Callers: []string{argoServer}, Why: "it answers the sign-ins for the API server"},
	{Workload: argoDex, Port: "grpc", Callers: []string{argoServer}, Why: "it gives the API server the identities of the users"},
	// The ApplicationSet controller runs the generators again for each
	// event, and Forgejo sends its events to the API server. The setting
	// appSetWebhookOpen is for installations that use this webhook.
	{Workload: argoAppSetController, Port: "webhook",
		Why: "Squared sends no events to it, and each event makes it run the generators of the ApplicationSets again"},
}

// selects tells if a label selector selects an object with the labels.
// An empty selector selects all objects.
func selects(sel kube.LabelSelector, labels map[string]string) bool {
	for k, v := range sel.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	for _, e := range sel.MatchExpressions {
		v, ok := labels[e.Key]
		switch e.Operator {
		case "In":
			if !ok || !slices.Contains(e.Values, v) {
				return false
			}
		case "NotIn":
			if ok && slices.Contains(e.Values, v) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// selectedNames returns the values of app.kubernetes.io/name that a pod
// selector permits, or nil when it permits other pods too.
func selectedNames(sel kube.LabelSelector) []string {
	return selectedValues(sel, "app.kubernetes.io/name")
}

// selectedValues returns the values of a label that a selector permits,
// or nil when it permits other objects too.
func selectedValues(sel kube.LabelSelector, key string) []string {
	var names []string
	if v, ok := sel.MatchLabels[key]; ok {
		names = []string{v}
	}
	for _, e := range sel.MatchExpressions {
		if e.Key != key {
			continue
		}
		if e.Operator != "In" {
			return nil
		}
		if names == nil {
			names = slices.Clone(e.Values)
		} else {
			names = slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(e.Values, n) })
		}
	}
	return names
}

// isIngress tells if a NetworkPolicy limits the connections to its pods.
func isIngress(p kube.NetworkPolicy) bool {
	if len(p.Spec.PolicyTypes) == 0 {
		// Kubernetes adds Ingress to each policy without policyTypes.
		return true
	}
	return slices.Contains(p.Spec.PolicyTypes, "Ingress")
}

// portMatches tells if a port of a rule (a number or a name, as JSON) is
// the container port (name, number).
func portMatches(raw json.RawMessage, name string, number int) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == name || s == strconv.Itoa(number)
	}
	var n int
	return json.Unmarshal(raw, &n) == nil && n == number
}

// checkProtectedPort returns the problems of the NetworkPolicies of the
// namespace (policies) for a protected port of a component. tpl is the pod
// template of the component. A pod can connect to the port when no policy
// limits the connections to the component, or when a rule of one policy
// permits it. The rules may permit only pods of the same namespace, with
// app.kubernetes.io/name of the callers.
func checkProtectedPort(pp protectedPort, tpl *kube.PodTemplate, policies []kube.NetworkPolicy) []string {
	number := 0
	for _, c := range tpl.Containers {
		for _, p := range c.Ports {
			if p.Name == pp.Port {
				number = p.ContainerPort
			}
		}
	}
	where := fmt.Sprintf("the port %s of %s", pp.Port, pp.Workload)
	if number == 0 && pp.Optional {
		return nil
	}
	if number == 0 {
		return []string{fmt.Sprintf("%s has no container port %s, thus the guard cannot check its NetworkPolicies", pp.Workload, pp.Port)}
	}
	var p []string
	limited := false
	for _, np := range policies {
		if !isIngress(np) || !selects(np.Spec.PodSelector, tpl.Labels) {
			continue
		}
		limited = true
		for i, rule := range np.Spec.Ingress {
			applies := len(rule.Ports) == 0
			for _, port := range rule.Ports {
				applies = applies || len(port.Port) == 0 || string(port.Port) == "null" || portMatches(port.Port, pp.Port, number)
			}
			if !applies {
				continue
			}
			rulePlace := fmt.Sprintf("rule %d of the NetworkPolicy %s", i+1, np.Metadata.Name)
			if len(rule.From) == 0 {
				p = append(p, fmt.Sprintf("%s permits each pod and each address to connect to %s", rulePlace, where))
				continue
			}
			for _, peer := range rule.From {
				switch {
				case len(peer.IPBlock) > 0 && string(peer.IPBlock) != "null":
					p = append(p, fmt.Sprintf("%s permits addresses (ipBlock) to connect to %s", rulePlace, where))
				case peer.NamespaceSelector != nil:
					names := selectedValues(*peer.NamespaceSelector, "kubernetes.io/metadata.name")
					switch {
					case names == nil && len(pp.Namespaces) == 0:
						p = append(p, fmt.Sprintf("%s permits pods of other namespaces to connect to %s", rulePlace, where))
					case names == nil:
						p = append(p, fmt.Sprintf("%s permits pods of other namespaces than %s to connect to %s", rulePlace, strings.Join(pp.Namespaces, ", "), where))
					default:
						for _, n := range names {
							if !slices.Contains(pp.Namespaces, n) {
								p = append(p, fmt.Sprintf("%s permits pods of the namespace %s to connect to %s", rulePlace, n, where))
							}
						}
					}
				case peer.PodSelector == nil:
					p = append(p, fmt.Sprintf("%s permits each pod to connect to %s", rulePlace, where))
				default:
					names := selectedNames(*peer.PodSelector)
					if names == nil {
						p = append(p, fmt.Sprintf("%s permits pods without a name of a component (app.kubernetes.io/name) to connect to %s", rulePlace, where))
						continue
					}
					for _, n := range names {
						switch {
						case slices.Contains(pp.Callers, n):
						case len(pp.Callers) == 0:
							p = append(p, fmt.Sprintf("%s permits %s to connect to %s, which no component needs", rulePlace, n, where))
						default:
							p = append(p, fmt.Sprintf("%s permits %s to connect to %s, which only %s need", rulePlace, n, where, strings.Join(pp.Callers, ", ")))
						}
					}
				}
			}
		}
	}
	if !limited {
		p = append(p, fmt.Sprintf("no NetworkPolicy limits the connections to %s, thus each pod, also of a tenant, can connect to %s", pp.Workload, where))
	}
	for i := range p {
		p[i] += ": " + pp.Why
	}
	return p
}
