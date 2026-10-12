package guard

import (
	"strings"
	"testing"
)

// chartAppSetController is the container of the ApplicationSet controller
// of the Argo CD Helm chart 10, with the SCM parameters.
const chartAppSetController = `[{"name": "applicationset-controller",
	"args": ["/usr/local/bin/argocd-applicationset-controller", "--metrics-addr=:8080"],
	"env": [
		{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS",
			"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "applicationsetcontroller.enable.scm.providers", "optional": true}}},
		{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS",
			"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "applicationsetcontroller.allowed.scm.providers", "optional": true}}},
		{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE",
			"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "applicationsetcontroller.enable.tokenref.strict.mode", "optional": true}}}]}]`

func TestCheckArgoSCM(t *testing.T) {
	chart := containersJSON(t, chartAppSetController)
	off := map[string]string{"applicationsetcontroller.enable.scm.providers": "false"}
	safeOn := map[string]string{"applicationsetcontroller.allowed.scm.providers": "https://git.example/api/v1, https://api.github.com",
		"applicationsetcontroller.enable.tokenref.strict.mode": "true"}

	for name, params := range map[string]map[string]string{"off": off, "on with limits": safeOn} {
		if p := checkArgoSCM(params, chart); len(p) != 0 {
			t.Errorf("%s: problems = %q", name, p)
		}
	}
	// Without the controller, no generator runs.
	if p := checkArgoSCM(nil, nil); len(p) != 0 {
		t.Errorf("problems = %q", p)
	}

	withArgs := func(args ...string) string {
		return strings.Replace(chartAppSetController, `"--metrics-addr=:8080"`, `"--metrics-addr=:8080", "`+strings.Join(args, `", "`)+`"`, 1)
	}
	tests := []struct {
		name       string
		params     map[string]string
		containers string
		want       string
	}{
		{"defaults", nil, chartAppSetController, "allowed.scm.providers is empty: an ApplicationSet can send tokens to each server"},
		{"defaults strict mode", nil, chartAppSetController, "enable.tokenref.strict.mode is not true"},
		{"no TLS", map[string]string{"applicationsetcontroller.allowed.scm.providers": "http://forgejo.localhost:30300",
			"applicationsetcontroller.enable.tokenref.strict.mode": "true"}, chartAppSetController, "has http://forgejo.localhost:30300 without TLS"},
		{"flag over the ConfigMap", off, withArgs("--enable-scm-providers"), "the SCM generators are on in container applicationset-controller"},
		{"last flag", off, withArgs("--enable-scm-providers=false", "--enable-scm-providers=true"), "the SCM generators are on"},
		{"flag for the list", safeOn, withArgs("--allowed-scm-providers", "http://x"), "has http://x without TLS"},
		{"variable as text", off, `[{"name": "c", "env": [{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS", "value": "true"}]}]`,
			"the SCM generators are on in container c"},
		{"variable from a Secret", off, `[{"name": "c", "env": [{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS",
			"valueFrom": {"secretKeyRef": {"name": "x", "key": "y"}}}]}]`, "container c reads ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS from another place"},
		{"bad value", map[string]string{"applicationsetcontroller.enable.scm.providers": "no"}, chartAppSetController, `has the value "no"`},
		{"envFrom", off, `[{"name": "c", "envFrom": [{"configMapRef": {"name": "x"}}]}]`, "reads environment variables with envFrom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkArgoSCM(tt.params, containersJSON(t, tt.containers))
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
