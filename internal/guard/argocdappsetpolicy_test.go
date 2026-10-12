package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// chartPolicyController is the container of the ApplicationSet controller
// of the Argo CD Helm chart 10, with the policy parameters.
const chartPolicyController = `[{"name": "applicationset-controller",
	"args": ["/usr/local/bin/argocd-applicationset-controller", "--metrics-addr=:8080"],
	"env": [
		{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_POLICY",
			"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "applicationsetcontroller.policy", "optional": true}}},
		{"name": "ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE",
			"valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "applicationsetcontroller.enable.policy.override", "optional": true}}}]}]`

func TestCheckArgoSetPolicy(t *testing.T) {
	chart := containersJSON(t, chartPolicyController)
	safe := map[string]string{"applicationsetcontroller.policy": "create-only", "applicationsetcontroller.enable.policy.override": "false"}
	c, p := checkArgoSetPolicy(safe, chart)
	if len(p) != 0 || c != (argoSetPolicy{Running: true, Policy: "create-only"}) {
		t.Fatalf("policy = %+v, problems = %q", c, p)
	}
	if c, p := checkArgoSetPolicy(map[string]string{"applicationsetcontroller.policy": "create-delete"}, chart); len(p) != 0 || !c.Override {
		t.Fatalf("policy = %+v, problems = %q", c, p)
	}
	// Without the controller.
	if c, p := checkArgoSetPolicy(nil, nil); len(p) != 0 || c.Running {
		t.Fatalf("policy = %+v, problems = %q", c, p)
	}

	withArgs := func(args ...string) string {
		return strings.Replace(chartPolicyController, `"--metrics-addr=:8080"`, `"--metrics-addr=:8080", "`+strings.Join(args, `", "`)+`"`, 1)
	}
	tests := []struct {
		name       string
		params     map[string]string
		containers string
		want       string
	}{
		{"defaults", nil, chartPolicyController, "has no applicationsetcontroller.policy, thus ApplicationSets use the policy sync"},
		{"sync", map[string]string{"applicationsetcontroller.policy": "sync"}, chartPolicyController, "has the policy sync"},
		{"create-update", map[string]string{"applicationsetcontroller.policy": "create-update"}, chartPolicyController, "has the policy create-update"},
		{"flag over the ConfigMap", safe, withArgs("--policy", "sync"), "has the policy sync"},
		{"unknown", map[string]string{"applicationsetcontroller.policy": "create"}, chartPolicyController, `has the value "create"`},
		{"bad override", map[string]string{"applicationsetcontroller.policy": "create-only", "applicationsetcontroller.enable.policy.override": "yes please"},
			chartPolicyController, `has the value "yes please" for applicationsetcontroller.enable.policy.override`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, p := checkArgoSetPolicy(tt.params, containersJSON(t, tt.containers))
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
	// A bool flag without a value turns the override on.
	if c, _ := checkArgoSetPolicy(safe, containersJSON(t, withArgs("--enable-policy-override"))); !c.Override {
		t.Fatalf("policy = %+v", c)
	}
}

func TestAppSetPolicyProblems(t *testing.T) {
	strict := argoSetPolicy{Running: true, Policy: "create-only"}
	override := argoSetPolicy{Running: true, Policy: "create-only", Override: true}
	tests := []struct {
		c    argoSetPolicy
		own  string
		want string
	}{
		{strict, "", ""},
		{strict, "sync", ""}, // The controller does not permit the override.
		{override, "", ""},
		{override, "create-delete", ""},
		{override, "sync", "its policy sync permits updates of its applications, because applicationsetcontroller.enable.policy.override is on"},
		{override, "create-update", "its policy create-update permits updates"},
		{override, "replace", `Argo CD does not know its policy "replace"`},
		// The controller itself is a problem, not each ApplicationSet.
		{argoSetPolicy{Running: true, Override: true}, "sync", ""},
		{argoSetPolicy{}, "sync", ""},
	}
	for _, tt := range tests {
		got := strings.Join(appSetPolicyProblems(tt.c, tt.own), " | ")
		if tt.want == "" && got != "" || !strings.Contains(got, tt.want) {
			t.Errorf("%+v %q: got %q, want %q", tt.c, tt.own, got, tt.want)
		}
	}

	// In checkArgoAppSets.
	set := appSet(t, func(s map[string]any) {
		s["syncPolicy"] = map[string]any{"applicationsSync": "sync", "preserveResourcesOnDeletion": true}
	})
	p, _, err := checkArgoAppSets([]json.RawMessage{set}, devAppRules, squaredNames, override)
	if err != nil || !strings.Contains(strings.Join(p, " | "), "the ApplicationSet team: its policy sync permits updates") {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}
