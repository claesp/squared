package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/kube"
)

// containersJSON returns the containers of a workload from JSON.
func containersJSON(t *testing.T, s string) []kube.Container {
	t.Helper()
	var c []kube.Container
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// chartController is the container of the application controller of the
// Argo CD Helm chart 10.
const chartController = `[{"name": "application-controller",
	"args": ["/usr/local/bin/argocd-application-controller", "--metrics-port=8082"],
	"env": [{"name": "ARGOCD_APPLICATION_NAMESPACES",
		"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "application.namespaces", "optional": true}}}]}]`

func TestCheckArgoNamespaces(t *testing.T) {
	chart := map[string][]kube.Container{"argocd-application-controller": containersJSON(t, chartController)}
	check := func(params map[string]string, containers map[string][]kube.Container, allowed []string) []string {
		values, problems := argoNamespaceValues(params, containers)
		return append(problems, checkArgoNamespaces(values, "argocd", allowed)...)
	}

	// The settings of the development environment, and the own namespace.
	if p := check(map[string]string{"server.insecure": "true"}, chart, nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	if p := check(map[string]string{"application.namespaces": "argocd"}, chart, nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	// Namespaces that an operator checked.
	if p := check(map[string]string{"application.namespaces": "team-a, team-b"}, chart, []string{"team-a", "team-b"}); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}

	tests := []struct {
		name       string
		params     map[string]string
		containers string
		want       string
	}{
		{"ConfigMap", map[string]string{"application.namespaces": "team-a,*"}, chartController,
			`application.namespaces (argocd-cmd-params-cm) has the namespace "*"`},
		{"ApplicationSets", map[string]string{"applicationsetcontroller.namespaces": "/^acme-.*$/"}, chartController,
			`applicationsetcontroller.namespaces (argocd-cmd-params-cm) has the namespace "/^acme-.*$/"`},
		{"flag", nil, `[{"name": "c", "args": ["argocd-application-controller", "--application-namespaces", "acme-*"]}]`,
			`application.namespaces (argocd-application-controller container c --application-namespaces) has the namespace "acme-*"`},
		{"flag with =", nil, `[{"name": "c", "command": ["argocd-application-controller", "--application-namespaces=x"]}]`,
			`has the namespace "x"`},
		{"variable", nil, `[{"name": "c", "env": [{"name": "ARGOCD_APPLICATION_NAMESPACES", "value": "x"}]}]`,
			`application.namespaces (argocd-application-controller container c ARGOCD_APPLICATION_NAMESPACES) has the namespace "x"`},
		{"variable from another ConfigMap", nil, `[{"name": "c", "env": [{"name": "ARGOCD_APPLICATION_NAMESPACES",
			"valueFrom": {"configMapKeyRef": {"name": "other", "key": "application.namespaces"}}}]}]`,
			"reads ARGOCD_APPLICATION_NAMESPACES from another place"},
		{"envFrom", nil, `[{"name": "c", "envFrom": [{"configMapRef": {"name": "other"}}]}]`,
			"reads environment variables with envFrom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := check(tt.params, map[string][]kube.Container{"argocd-application-controller": containersJSON(t, tt.containers)}, []string{"team-a"})
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
}

func TestJoinSentences(t *testing.T) {
	got := joinSentences([]string{"the a are not safe: x. The guard keeps the current protection", "the b changed"})
	if want := "the a are not safe: x. The guard keeps the current protection. The b changed"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
