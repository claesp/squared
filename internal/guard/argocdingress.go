package guard

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"squared/internal/kube"
)

// serverInsecure is the parameter that makes the API server speak HTTP
// without TLS, for TLS at an Ingress in front of it.
var serverInsecure = argoParam{"server.insecure", "ARGOCD_SERVER_INSECURE", "--insecure"}

// argoIngress is what the guard needs to check how Argo CD is reachable
// from outside its namespace.
type argoIngress struct {
	Namespace string
	// Templates are the pod templates of the components that run.
	Templates map[string]*kube.PodTemplate
	// Insecure tells that the API server speaks HTTP without TLS.
	Insecure bool
	// PlainHTTP are the Services that may publish the API server with
	// HTTP: for example a NodePort only on 127.0.0.1.
	PlainHTTP []string
	// WebhookOpen permits the webhook of the ApplicationSet controller.
	WebhookOpen bool
}

// exposable tells if a port of a component may be reachable from outside
// the namespace: only the port server of the API server, and the webhook
// of the ApplicationSet controller with appSetWebhookOpen.
func (a argoIngress) exposable(component, port string) bool {
	return component == argoServer && port == "server" ||
		component == argoAppSetController && port == "webhook" && a.WebhookOpen
}

// components returns the components whose pods a Service selects.
func (a argoIngress) components(svc kube.Service) []string {
	if len(svc.Spec.Selector) == 0 {
		return nil
	}
	var c []string
	for _, name := range slices.Sorted(maps.Keys(a.Templates)) {
		tpl := a.Templates[name]
		if tpl != nil && selects(kube.LabelSelector{MatchLabels: svc.Spec.Selector}, tpl.Labels) {
			c = append(c, name)
		}
	}
	return c
}

// containerPort returns the name of the container port of a component
// that a port of a Service (its number or its name) sends to, or "" when
// the guard cannot find it.
func containerPort(svc kube.Service, port string, tpl *kube.PodTemplate) string {
	for _, sp := range svc.Spec.Ports {
		if port != sp.Name && port != strconv.Itoa(sp.Port) {
			continue
		}
		var target string
		var number int
		switch {
		case len(sp.TargetPort) == 0 || string(sp.TargetPort) == "null":
			number = sp.Port
		case json.Unmarshal(sp.TargetPort, &target) == nil:
		case json.Unmarshal(sp.TargetPort, &number) == nil:
		}
		for _, c := range tpl.Containers {
			for _, p := range c.Ports {
				if target != "" && p.Name == target || number != 0 && p.ContainerPort == number {
					return cmpOrPort(p.Name, p.ContainerPort)
				}
			}
		}
		return cmpOrPort(target, number)
	}
	return ""
}

// cmpOrPort returns the name of a port, or its number without a name.
func cmpOrPort(name string, number int) string {
	if name != "" {
		return name
	}
	return strconv.Itoa(number)
}

// reaches returns the problems of a Service port that is reachable from
// outside the namespace (where): each component port that it reaches and
// that must not be reachable. It also tells if it reaches the API server.
func (a argoIngress) reaches(where string, svc kube.Service, port string) (problems []string, server bool) {
	for _, c := range a.components(svc) {
		cp := containerPort(svc, port, a.Templates[c])
		switch {
		case a.exposable(c, cp):
			server = server || c == argoServer
		case cp == "":
			problems = append(problems, fmt.Sprintf("%s sends requests to the port %s of the Service %s, which the guard cannot find", where, port, svc.Metadata.Name))
		default:
			problems = append(problems, fmt.Sprintf("%s makes the port %s of %s (Service %s) reachable from outside the namespace %s, but only the port server of the API server may be",
				where, cp, c, svc.Metadata.Name, a.Namespace))
		}
	}
	return problems, server
}

// checkArgoIngress returns the problems of the ways from outside the
// namespace of Argo CD to its components: the Ingresses of the namespace,
// the HTTPRoutes of all namespaces, the Services of the namespace that are
// reachable from outside the cluster (NodePort, LoadBalancer, externalIPs),
// and the Services of type ExternalName in all namespaces (services), which
// an Ingress in another namespace can use. Only the port server of the API
// server may be reachable: the repository server, Redis, the commit server,
// Dex and the metrics do not check who connects. An Ingress to the API
// server must have TLS: the web interface and the API carry passwords,
// tokens and session cookies.
func checkArgoIngress(a argoIngress, services []kube.Service, ingresses []kube.Ingress, routes []kube.HTTPRoute) []string {
	own := map[string]kube.Service{}
	for _, s := range services {
		if s.Metadata.Namespace == a.Namespace {
			own[s.Metadata.Name] = s
		}
	}
	var p []string
	for _, in := range ingresses {
		for _, b := range in.Backends {
			svc, ok := own[b.Name]
			if !ok {
				continue
			}
			bp, server := a.reaches("the Ingress "+in.Name, svc, b.Port)
			p = append(p, bp...)
			tls := slices.Contains(in.TLS, b.Host) || slices.Contains(in.TLS, "") || b.Host == "" && len(in.TLS) > 0
			if server && !tls {
				host := b.Host
				if host == "" {
					host = "each host"
				}
				p = append(p, fmt.Sprintf("the Ingress %s sends requests for %s to the API server without TLS: passwords, tokens and session cookies go as plain text", in.Name, host))
			}
		}
	}
	for _, r := range routes {
		for _, b := range r.Backends {
			ns := b.Namespace
			if ns == "" {
				ns = r.Namespace
			}
			if svc, ok := own[b.Name]; ok && ns == a.Namespace {
				bp, _ := a.reaches(fmt.Sprintf("the HTTPRoute %s/%s", r.Namespace, r.Name), svc, b.Port)
				p = append(p, bp...)
			}
		}
	}
	for _, svc := range services {
		switch {
		case svc.Metadata.Namespace == a.Namespace && (svc.Spec.Type == "NodePort" || svc.Spec.Type == "LoadBalancer" || len(svc.Spec.ExternalIPs) > 0):
			where := fmt.Sprintf("the Service %s (%s)", svc.Metadata.Name, cmpOrType(svc))
			for _, sp := range svc.Spec.Ports {
				bp, server := a.reaches(where, svc, strconv.Itoa(sp.Port))
				p = append(p, bp...)
				if server && a.Insecure && !slices.Contains(a.PlainHTTP, svc.Metadata.Name) {
					p = append(p, fmt.Sprintf("%s publishes the port %d of the API server outside the cluster with HTTP without TLS (server.insecure): passwords, tokens and session cookies go as plain text",
						where, sp.Port))
				}
			}
		case svc.Spec.Type == "ExternalName":
			target, ok := own[externalTarget(svc.Spec.ExternalName, a.Namespace)]
			if !ok {
				continue
			}
			where := fmt.Sprintf("the Service %s/%s of type ExternalName", svc.Metadata.Namespace, svc.Metadata.Name)
			for _, sp := range target.Spec.Ports {
				bp, _ := a.reaches(where, target, strconv.Itoa(sp.Port))
				p = append(p, bp...)
			}
		}
	}
	return p
}

// cmpOrType returns the type of a Service for the messages.
func cmpOrType(svc kube.Service) string {
	if len(svc.Spec.ExternalIPs) > 0 && svc.Spec.Type != "NodePort" && svc.Spec.Type != "LoadBalancer" {
		return "externalIPs"
	}
	return svc.Spec.Type
}

// externalTarget returns the name of the Service in the namespace that an
// ExternalName points to, for example "argocd-redis" for
// "argocd-redis.argocd.svc.cluster.local", or "".
func externalTarget(name, namespace string) string {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for _, suffix := range []string{"." + namespace + ".svc.cluster.local", "." + namespace + ".svc"} {
		if svc, ok := strings.CutSuffix(name, suffix); ok && !strings.Contains(svc, ".") {
			return svc
		}
	}
	return ""
}
