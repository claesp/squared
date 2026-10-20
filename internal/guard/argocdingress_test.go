package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/kube"
)

func ingressSetup(t *testing.T) argoIngress {
	t.Helper()
	tpl := func(name string, ports map[string]int) *kube.PodTemplate {
		return &kube.PodTemplate{Labels: map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/instance": "argocd"},
			Containers: containersFromPorts(ports)}
	}
	return argoIngress{Namespace: "argocd", Insecure: true, Templates: map[string]*kube.PodTemplate{
		argoServer:           tpl(argoServer, map[string]int{"server": 8080, "metrics": 8083}),
		argoRepoServer:       tpl(argoRepoServer, map[string]int{"repo-server": 8081, "metrics": 8084}),
		argoRedis:            tpl(argoRedis, map[string]int{"redis": 6379}),
		argoAppSetController: tpl(argoAppSetController, map[string]int{"metrics": 8080, "probe": 8081, "webhook": 7000}),
	}}
}

// devServices are the Services of the development environment, with the
// NodePort of the API server.
const devServices = `[
	{"metadata": {"name": "argocd-server", "namespace": "argocd"}, "spec": {"type": "NodePort",
		"selector": {"app.kubernetes.io/name": "argocd-server", "app.kubernetes.io/instance": "argocd"},
		"ports": [{"name": "http", "port": 80, "targetPort": 8080}, {"name": "https", "port": 443, "targetPort": 8080}]}},
	{"metadata": {"name": "argocd-server-metrics", "namespace": "argocd"}, "spec": {"type": "ClusterIP",
		"selector": {"app.kubernetes.io/name": "argocd-server"}, "ports": [{"name": "http-metrics", "port": 8083, "targetPort": "metrics"}]}},
	{"metadata": {"name": "argocd-repo-server", "namespace": "argocd"}, "spec": {"type": "ClusterIP",
		"selector": {"app.kubernetes.io/name": "argocd-repo-server"}, "ports": [{"name": "tcp-repo-server", "port": 8081, "targetPort": "repo-server"}]}},
	{"metadata": {"name": "argocd-redis", "namespace": "argocd"}, "spec": {"type": "ClusterIP",
		"selector": {"app.kubernetes.io/name": "argocd-redis"}, "ports": [{"name": "tcp-redis", "port": 6379, "targetPort": 6379}]}},
	{"metadata": {"name": "argocd-applicationset-controller", "namespace": "argocd"}, "spec": {"type": "ClusterIP",
		"selector": {"app.kubernetes.io/name": "argocd-applicationset-controller"}, "ports": [{"name": "http-webhook", "port": 7000, "targetPort": "webhook"}]}},
	{"metadata": {"name": "web", "namespace": "acme-dev"}, "spec": {"type": "ClusterIP", "selector": {"app": "web"}, "ports": [{"port": 80}]}}]`

func services(t *testing.T, s string) []kube.Service {
	t.Helper()
	var out []kube.Service
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCheckArgoIngress(t *testing.T) {
	a := ingressSetup(t)
	dev := services(t, devServices)

	// The NodePort with HTTP is a problem, except as in the development
	// environment, where Kind publishes it only on 127.0.0.1.
	p := checkArgoIngress(a, dev, nil, nil)
	if len(p) != 2 || !strings.Contains(p[0], "the Service argocd-server (NodePort) publishes the port 80 of the API server outside the cluster with HTTP without TLS (server.insecure)") {
		t.Fatalf("problems = %q", p)
	}
	a.PlainHTTP = []string{"argocd-server"}
	if p := checkArgoIngress(a, dev, nil, nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// With TLS in the API server, the NodePort is no problem.
	b := ingressSetup(t)
	b.Insecure = false
	if p := checkArgoIngress(b, dev, nil, nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	clusterIP := services(t, strings.ReplaceAll(devServices, `"type": "NodePort"`, `"type": "ClusterIP"`))
	ingress := func(backends []kube.IngressBackend, tls ...string) []kube.Ingress {
		return []kube.Ingress{{Name: "argocd", Backends: backends, TLS: tls}}
	}
	backend := func(svc, port, host string) kube.IngressBackend {
		return kube.IngressBackend{ServiceBackend: kube.ServiceBackend{Name: svc, Port: port}, Host: host}
	}
	// An Ingress with TLS for the API server.
	if p := checkArgoIngress(a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-server", "80", "argocd.example")}, "argocd.example"), nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	tests := []struct {
		name      string
		a         argoIngress
		services  []kube.Service
		ingresses []kube.Ingress
		routes    []kube.HTTPRoute
		want      string
	}{
		{"no TLS", a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-server", "http", "argocd.example")}), nil,
			"the Ingress argocd sends requests for argocd.example to the API server without TLS"},
		{"TLS for another host", a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-server", "80", "argocd.example")}, "other.example"), nil,
			"sends requests for argocd.example to the API server without TLS"},
		{"metrics", a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-server-metrics", "8083", "metrics.example")}, "metrics.example"), nil,
			"the Ingress argocd makes the port metrics of argocd-server (Service argocd-server-metrics) reachable from outside the namespace argocd"},
		{"repository server", a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-repo-server", "8081", "")}, ""), nil,
			"makes the port repo-server of argocd-repo-server (Service argocd-repo-server) reachable"},
		{"webhook", a, clusterIP, ingress([]kube.IngressBackend{backend("argocd-applicationset-controller", "7000", "hooks.example")}, "hooks.example"), nil,
			"makes the port webhook of argocd-applicationset-controller"},
		{"HTTPRoute to Redis", a, clusterIP, nil, []kube.HTTPRoute{{Name: "cache", Namespace: "acme-dev",
			Backends: []kube.ServiceBackend{{Name: "argocd-redis", Namespace: "argocd", Port: "6379"}}}},
			"the HTTPRoute acme-dev/cache makes the port redis of argocd-redis (Service argocd-redis) reachable"},
		{"NodePort of the repository server", a, services(t, strings.Replace(devServices,
			`"name": "argocd-repo-server", "namespace": "argocd"}, "spec": {"type": "ClusterIP"`,
			`"name": "argocd-repo-server", "namespace": "argocd"}, "spec": {"type": "NodePort"`, 1)), nil, nil,
			"the Service argocd-repo-server (NodePort) makes the port repo-server of argocd-repo-server"},
		{"ExternalName", a, append(clusterIP, services(t, `[{"metadata": {"name": "cache", "namespace": "acme-dev"},
			"spec": {"type": "ExternalName", "externalName": "argocd-repo-server.argocd.svc.cluster.local."}}]`)...), nil, nil,
			"the Service acme-dev/cache of type ExternalName makes the port repo-server of argocd-repo-server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkArgoIngress(tt.a, tt.services, tt.ingresses, tt.routes)
			if !strings.Contains(strings.Join(p, " | "), tt.want) {
				t.Fatalf("problems = %q, want %q", p, tt.want)
			}
			for _, m := range p {
				if strings.Contains(m, ";") {
					t.Errorf("the message has a semicolon: %q", m)
				}
			}
		})
	}

	// The webhook with appSetWebhookOpen, an ExternalName to the API server,
	// and an Ingress of a tenant Service.
	open := a
	open.WebhookOpen = true
	extra := append(clusterIP, services(t, `[{"metadata": {"name": "ui", "namespace": "acme-dev"},
		"spec": {"type": "ExternalName", "externalName": "argocd-server.argocd.svc"}}]`)...)
	if p := checkArgoIngress(open, extra, ingress([]kube.IngressBackend{backend("argocd-applicationset-controller", "7000", "hooks.example"),
		backend("web", "80", "web.example")}), nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}
