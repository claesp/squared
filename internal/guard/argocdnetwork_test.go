package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/kube"
)

// chartPolicies are the NetworkPolicies of the Argo CD Helm chart 10 for
// the repository server and Redis.
const chartPolicies = `[
	{"metadata": {"name": "argocd-repo-server"}, "spec": {
		"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-repo-server", "app.kubernetes.io/instance": "argocd"}},
		"policyTypes": ["Ingress"],
		"ingress": [
			{"from": [
				{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-server", "app.kubernetes.io/instance": "argocd"}}},
				{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-application-controller", "app.kubernetes.io/instance": "argocd"}}},
				{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-applicationset-controller", "app.kubernetes.io/instance": "argocd"}}}],
			 "ports": [{"port": "repo-server", "protocol": "TCP"}]},
			{"from": [{"namespaceSelector": {}}], "ports": [{"port": "metrics"}]}]}},
	{"metadata": {"name": "argocd-redis"}, "spec": {
		"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-redis", "app.kubernetes.io/instance": "argocd"}},
		"policyTypes": ["Ingress"],
		"ingress": [{"from": [
			{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-server", "app.kubernetes.io/instance": "argocd"}}},
			{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-repo-server", "app.kubernetes.io/instance": "argocd"}}},
			{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-application-controller", "app.kubernetes.io/instance": "argocd"}}}],
		 "ports": [{"port": "redis", "protocol": "TCP"}]}]}}]`

func policies(t *testing.T, s string) []kube.NetworkPolicy {
	t.Helper()
	var p []kube.NetworkPolicy
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

var repoTemplate = &kube.PodTemplate{
	Labels:     map[string]string{"app.kubernetes.io/name": "argocd-repo-server", "app.kubernetes.io/instance": "argocd"},
	Containers: containersFromPorts(map[string]int{"repo-server": 8081, "metrics": 8084}),
}

func containersFromPorts(ports map[string]int) []kube.Container {
	var c kube.Container
	c.Name = "c"
	for name, number := range ports {
		c.Ports = append(c.Ports, struct {
			Name          string `json:"name"`
			ContainerPort int    `json:"containerPort"`
		}{name, number})
	}
	return []kube.Container{c}
}

func TestCheckProtectedPort(t *testing.T) {
	repo, redis := protectedPorts[0], protectedPorts[1]
	redisTemplate := &kube.PodTemplate{Labels: map[string]string{"app.kubernetes.io/name": "argocd-redis", "app.kubernetes.io/instance": "argocd"},
		Containers: containersFromPorts(map[string]int{"redis": 6379, "metrics": 9121})}
	if p := checkProtectedPort(repo, repoTemplate, policies(t, chartPolicies)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	if p := checkProtectedPort(redis, redisTemplate, policies(t, chartPolicies)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// A policy that denies all connections, and an egress policy.
	if p := checkProtectedPort(repo, repoTemplate, policies(t, `[{"metadata": {"name": "deny"}, "spec": {"podSelector": {}, "policyTypes": ["Ingress"]}}]`)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	extra := func(rule string) []kube.NetworkPolicy {
		return append(policies(t, chartPolicies), policies(t, `[{"metadata": {"name": "extra"}, "spec": {
			"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-repo-server"}}, "ingress": [`+rule+`]}}]`)...)
	}
	tests := []struct {
		name     string
		policies []kube.NetworkPolicy
		want     string
	}{
		{"no policy", nil, "no NetworkPolicy limits the connections to argocd-repo-server, thus each pod, also of a tenant, can connect to the port repo-server of argocd-repo-server"},
		{"only egress", policies(t, `[{"metadata": {"name": "out"}, "spec": {"podSelector": {}, "policyTypes": ["Egress"]}}]`), "no NetworkPolicy limits"},
		{"all sources", extra(`{}`), "rule 1 of the NetworkPolicy extra permits each pod and each address to connect to the port repo-server of argocd-repo-server: it renders the manifests"},
		{"other namespaces", extra(`{"from": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "acme-dev"}}}]}`), "permits pods of other namespaces"},
		{"addresses", extra(`{"from": [{"ipBlock": {"cidr": "10.0.0.0/8"}}], "ports": [{"port": 8081}]}`), "permits addresses (ipBlock)"},
		{"all pods of the namespace", extra(`{"from": [{"podSelector": {}}], "ports": [{"port": "repo-server"}]}`), "permits pods without a name of a component"},
		{"another component", extra(`{"from": [{"podSelector": {"matchExpressions": [{"key": "app.kubernetes.io/name", "operator": "In", "values": ["argocd-server", "argocd-dex-server"]}]}}]}`),
			"permits argocd-dex-server to connect to the port repo-server of argocd-repo-server, which only argocd-server, argocd-application-controller"},
		{"no container port", nil, "has no container port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl := repoTemplate
			if tt.name == "no container port" {
				tpl = &kube.PodTemplate{Labels: repoTemplate.Labels}
			}
			p := checkProtectedPort(repo, tpl, tt.policies)
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
	// A rule for the metrics port only does not open the protected port.
	if p := checkProtectedPort(repo, repoTemplate, extra(`{"from": [{"ipBlock": {"cidr": "0.0.0.0/0"}}], "ports": [{"port": "metrics"}]}`)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}

func TestSelects(t *testing.T) {
	var sel kube.LabelSelector
	if err := json.Unmarshal([]byte(`{"matchLabels": {"a": "1"}, "matchExpressions": [{"key": "b", "operator": "NotIn", "values": ["x"]}, {"key": "c", "operator": "Exists"}]}`), &sel); err != nil {
		t.Fatal(err)
	}
	if !selects(sel, map[string]string{"a": "1", "b": "y", "c": ""}) || selects(sel, map[string]string{"a": "1", "b": "x", "c": ""}) ||
		selects(sel, map[string]string{"a": "1"}) || !selects(kube.LabelSelector{}, nil) {
		t.Fatal("selects is not correct")
	}
}

func TestCheckAppSetWebhookPort(t *testing.T) {
	webhook := protectedPorts[len(protectedPorts)-1]
	if webhook.Workload != argoAppSetController || webhook.Port != "webhook" {
		t.Fatalf("the last protected port is %+v", webhook)
	}
	tpl := &kube.PodTemplate{
		Labels:     map[string]string{"app.kubernetes.io/name": "argocd-applicationset-controller", "app.kubernetes.io/instance": "argocd"},
		Containers: containersFromPorts(map[string]int{"metrics": 8080, "probe": 8081, "webhook": 7000}),
	}
	// The policy of the Helm chart with metrics.
	chart := `[{"metadata": {"name": "argocd-applicationset-controller"}, "spec": {
		"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-applicationset-controller", "app.kubernetes.io/instance": "argocd"}},
		"policyTypes": ["Ingress"], "ingress": [{"from": [{"namespaceSelector": {}}], "ports": [{"port": "metrics"}]}]}}]`
	if p := checkProtectedPort(webhook, tpl, policies(t, chart)); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// Without metrics, the chart makes no policy.
	if p := checkProtectedPort(webhook, tpl, nil); len(p) != 1 || !strings.Contains(p[0], "no NetworkPolicy limits the connections to argocd-applicationset-controller") {
		t.Fatalf("problems = %q", p)
	}
	// With an Ingress, the chart opens the webhook.
	open := `[{"metadata": {"name": "argocd-applicationset-controller"}, "spec": {
		"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-applicationset-controller"}},
		"ingress": [{"ports": [{"port": "webhook"}]}, {"from": [{"namespaceSelector": {}}], "ports": [{"port": "metrics"}]}]}}]`
	if p := checkProtectedPort(webhook, tpl, policies(t, open)); len(p) != 1 || !strings.Contains(p[0], "permits each pod and each address to connect to the port webhook of argocd-applicationset-controller: Squared sends no events to it") {
		t.Fatalf("problems = %q", p)
	}
	// Also the API server needs no connection.
	server := `[{"metadata": {"name": "x"}, "spec": {"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-applicationset-controller"}},
		"ingress": [{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "argocd-server"}}}], "ports": [{"port": 7000}]}]}}]`
	if p := checkProtectedPort(webhook, tpl, policies(t, server)); len(p) != 1 || !strings.Contains(p[0], "permits argocd-server to connect to the port webhook of argocd-applicationset-controller, which no component needs") {
		t.Fatalf("problems = %q", p)
	}
}
