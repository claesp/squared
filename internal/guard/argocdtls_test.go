package guard

import (
	"strings"
	"testing"

	"squared/internal/kube"
)

// tlsTemplates are the components of the development environment. Their
// containers read the parameters from argocd-cmd-params-cm, as in the Helm
// chart.
func tlsTemplates(t *testing.T, extra map[string]string) map[string]*kube.PodTemplate {
	t.Helper()
	ref := func(env, key string) string {
		return `{"name": "` + env + `", "valueFrom": {"configMapKeyRef": {"name": "argocd-cmd-params-cm", "key": "` + key + `", "optional": true}}}`
	}
	tpl := func(env ...string) *kube.PodTemplate {
		return &kube.PodTemplate{Containers: containersJSON(t, `[{"name": "c", "args": ["`+extra["args"]+`"], "env": [`+strings.Join(env, ",")+`]}]`)}
	}
	return map[string]*kube.PodTemplate{
		argoServer: tpl(ref("ARGOCD_SERVER_REPO_SERVER_PLAINTEXT", "server.repo.server.plaintext"),
			ref("ARGOCD_SERVER_REPO_SERVER_STRICT_TLS", "server.repo.server.strict.tls"),
			ref("ARGOCD_SERVER_DEX_SERVER_PLAINTEXT", "server.dex.server.plaintext"),
			ref("ARGOCD_SERVER_DEX_SERVER_STRICT_TLS", "server.dex.server.strict.tls"),
			ref("ARGOCD_TLS_MIN_VERSION", "server.tls.minversion")),
		argoAppController: tpl(ref("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT", "controller.repo.server.plaintext"),
			ref("ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_STRICT_TLS", "controller.repo.server.strict.tls")),
		argoAppSetController: tpl(ref("ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER_PLAINTEXT", "applicationsetcontroller.repo.server.plaintext"),
			ref("ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER_STRICT_TLS", "applicationsetcontroller.repo.server.strict.tls")),
		argoRepoServer: tpl(ref("ARGOCD_REPO_SERVER_DISABLE_TLS", "reposerver.disable.tls"), ref("ARGOCD_TLS_MIN_VERSION", "reposerver.tls.minversion")),
	}
}

var devTLSParams = map[string]string{"server.insecure": "true", "server.repo.server.strict.tls": "true",
	"controller.repo.server.strict.tls": "true", "applicationsetcontroller.repo.server.strict.tls": "true"}

const devIssuer = "http://keycloak.localhost:30180/realms/squared"

var devArgoCM = map[string]string{"oidc.config": "name: Keycloak\nissuer: " + devIssuer + "\nclientID: argocd\n"}

func TestCheckArgoTLS(t *testing.T) {
	if p := checkArgoTLS(devTLSParams, tlsTemplates(t, nil), devArgoCM, []string{devIssuer}); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range devTLSParams {
			m[a] = b
		}
		m[k] = v
		return m
	}
	tests := []struct {
		name    string
		params  map[string]string
		args    string
		cm      map[string]string
		issuers []string
		want    string
	}{
		{"defaults of the chart", map[string]string{"server.insecure": "true"}, "", devArgoCM, []string{devIssuer},
			"argocd-application-controller does not check the TLS certificate of the repository server (controller.repo.server.strict.tls is not true)"},
		{"plain text", with("server.repo.server.plaintext", "true"), "", devArgoCM, []string{devIssuer},
			"argocd-server connects to the repository server without TLS (server.repo.server.plaintext): the credentials of the repositories go as plain text"},
		{"flag", devTLSParams, "--repo-server-plaintext", devArgoCM, []string{devIssuer}, "connects to the repository server without TLS"},
		{"no TLS in the repository server", with("reposerver.disable.tls", "true"), "", devArgoCM, []string{devIssuer},
			"argocd-repo-server serves without TLS (reposerver.disable.tls)"},
		{"old TLS", with("reposerver.tls.minversion", "1.0"), "", devArgoCM, []string{devIssuer},
			"argocd-repo-server accepts TLS 1.0 (reposerver.tls.minversion)"},
		{"bad value", with("controller.repo.server.strict.tls", "yes"), "", devArgoCM, []string{devIssuer}, `has the value "yes"`},
		{"no check of the identity provider", devTLSParams, "", map[string]string{"oidc.tls.insecure.skip.verify": "true"}, nil,
			"argocd-cm has oidc.tls.insecure.skip.verify"},
		{"issuer without TLS", devTLSParams, "", devArgoCM, nil,
			"the issuer " + devIssuer + " in oidc.config of argocd-cm has no TLS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkArgoTLS(tt.params, tlsTemplates(t, map[string]string{"args": tt.args}), tt.cm, tt.issuers)
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

	// Dex: only when it runs.
	templates := tlsTemplates(t, nil)
	templates[argoDex] = &kube.PodTemplate{Containers: containersJSON(t, `[{"name": "dex"}]`)}
	p := checkArgoTLS(devTLSParams, templates, devArgoCM, []string{devIssuer})
	if len(p) != 1 || !strings.Contains(p[0], "argocd-server does not check the TLS certificate of Dex (server.dex.server.strict.tls is not true)") {
		t.Fatalf("problems = %q", p)
	}
	// Without the repository server, its clients do not count.
	delete(templates, argoRepoServer)
	delete(templates, argoDex)
	if p := checkArgoTLS(map[string]string{}, templates, nil, nil); len(p) != 0 {
		t.Fatalf("problems = %q", p)
	}
}
