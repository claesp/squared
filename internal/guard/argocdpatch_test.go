package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckTemplatePatchSafe(t *testing.T) {
	for _, patch := range []string{
		"",
		// Examples of the documentation of Argo CD.
		`spec:
  source:
    helm:
      valueFiles:
      {{- range $valueFile := .valueFiles }}
        - {{ $valueFile }}
      {{- end }}
{{- if .autoSync }}
  syncPolicy:
    automated:
      prune: {{ .prune }}
{{- end }}
`,
		`{"metadata": {"labels": {"team": "{{ .team }}"}}, "spec": {"project": "team"}}`,
		"{{/* A comment. */}}\n{{ $env := .env }}\nmetadata:\n  annotations:\n    env: \"{{ $env }}\"\n",
	} {
		p, err := checkTemplatePatch(patch, devProjectRules.RepoPrefixes, squaredNames)
		if err != nil || len(p) != 0 {
			t.Errorf("patch %q: problems = %q, err %v", patch, p, err)
		}
	}
}

func TestCheckTemplatePatch(t *testing.T) {
	tests := []struct {
		name, patch, want string
	}{
		{"tenant project", "spec:\n  project: tenant-acme\n", "in its templatePatch, its applications use the tenant project tenant-acme"},
		{"project in a branch", "spec:\n{{- if .x }}\n  project: tenant-acme\n{{- end }}\n  destination:\n    namespace: x\n", "tenant project tenant-acme"},
		{"project from the parameters", "spec:\n  project: {{ .project }}\n", "comes from the generators"},
		{"project in quotes", "spec:\n  project: \"{{ .project }}\"\n", "comes from the generators"},
		{"GitOps repository", "spec:\n  sources:\n    - repoURL: http://forgejo.localhost:30300/squared/platform\n      ref: values\n",
			"its applications use the GitOps repository http://forgejo.localhost:30300/squared/platform"},
		{"repository from the parameters", "spec:\n  source:\n    repoURL: '{{ .url }}'\n", "the repository"},
		{"label of Squared", "metadata:\n  labels:\n    squared.dev/managed: \"true\"\n", "the label squared.dev/managed"},
		{"name of Squared", "metadata:\n  name: acme-dev-shop\n", "it makes the application acme-dev-shop"},
		{"finalizer", "metadata:\n  finalizers:\n    - resources-finalizer.argocd.argoproj.io\n", "the finalizer resources-finalizer.argocd.argoproj.io"},
		{"key from the parameters", "spec:\n  {{ .field }}: tenant-acme\n", `the key "{{...}}" comes from the parameters`},
		{"label key from the parameters", "metadata:\n  labels:\n    {{ .k }}: \"true\"\n", "comes from the parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := checkTemplatePatch(tt.patch, devProjectRules.RepoPrefixes, squaredNames)
			if err != nil || !strings.Contains(strings.Join(p, " | "), tt.want) {
				t.Fatalf("problems = %q, err %v, want %q", p, err, tt.want)
			}
		})
	}
}

func TestCheckTemplatePatchUnreadable(t *testing.T) {
	for _, patch := range []string{
		"spec:\n{{ toYaml .spec | nindent 2 }}\n",
		"{{ template \"x\" . }}\n",
		"spec:\n  project: {{ if .x\n  }}a{{ end }}\n",
		"spec: [\n",
		// Both branches have the key: the last value must not hide the
		// first.
		"spec:\n{{- if .x }}\n  project: tenant-acme\n{{- else }}\n  project: team\n{{- end }}\n",
	} {
		if _, err := checkTemplatePatch(patch, devProjectRules.RepoPrefixes, squaredNames); err == nil {
			t.Errorf("patch %q: no error", patch)
		}
	}
	// In checkArgoAppSets, the guard reports it.
	set := appSet(t, func(s map[string]any) { s["templatePatch"] = "spec:\n{{ toYaml .spec | nindent 2 }}\n" })
	p, _, _, err := checkArgoAppSets([]json.RawMessage{set}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || !strings.Contains(strings.Join(p, " | "), "the guard cannot read its templatePatch (a line writes text from the parameters") {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}
