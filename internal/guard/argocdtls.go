package guard

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"squared/internal/kube"
)

// tlsClient is a component of Argo CD that connects to another component,
// with the parameters that turn TLS off (Plaintext) and that check the
// certificate (Strict, nil when there is none).
type tlsClient struct {
	Workload          string
	Plaintext, Strict argoParam
	HasStrict         bool
}

// repoServerClients are the components that connect to the repository
// server. They send the credentials of the repositories with each request.
var repoServerClients = []tlsClient{
	{argoServer, argoParam{"server.repo.server.plaintext", "ARGOCD_SERVER_REPO_SERVER_PLAINTEXT", "--repo-server-plaintext"},
		argoParam{"server.repo.server.strict.tls", "ARGOCD_SERVER_REPO_SERVER_STRICT_TLS", "--repo-server-strict-tls"}, true},
	{argoAppController, argoParam{"controller.repo.server.plaintext", "ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_PLAINTEXT", "--repo-server-plaintext"},
		argoParam{"controller.repo.server.strict.tls", "ARGOCD_APPLICATION_CONTROLLER_REPO_SERVER_STRICT_TLS", "--repo-server-strict-tls"}, true},
	{argoAppSetController, argoParam{"applicationsetcontroller.repo.server.plaintext", "ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER_PLAINTEXT", "--repo-server-plaintext"},
		argoParam{"applicationsetcontroller.repo.server.strict.tls", "ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER_STRICT_TLS", "--repo-server-strict-tls"}, true},
	{argoNotifications, argoParam{"notificationscontroller.repo.server.plaintext", "ARGOCD_NOTIFICATION_CONTROLLER_REPO_SERVER_PLAINTEXT", "--argocd-repo-server-plaintext"},
		argoParam{}, false},
}

// dexClient is the API server, which gets the identities of the users from
// Dex.
var dexClient = tlsClient{argoServer, argoParam{"server.dex.server.plaintext", "ARGOCD_SERVER_DEX_SERVER_PLAINTEXT", "--dex-server-plaintext"},
	argoParam{"server.dex.server.strict.tls", "ARGOCD_SERVER_DEX_SERVER_STRICT_TLS", "--dex-server-strict-tls"}, true}

// tlsServer is a component that serves TLS, with the parameter that turns
// it off and the parameter of the lowest TLS version.
type tlsServer struct {
	Workload            string
	Disable, MinVersion argoParam
	HasDisable          bool
}

var tlsServers = []tlsServer{
	{argoServer, argoParam{}, argoParam{"server.tls.minversion", "ARGOCD_TLS_MIN_VERSION", "--tlsminversion"}, false},
	{argoRepoServer, argoParam{"reposerver.disable.tls", "ARGOCD_REPO_SERVER_DISABLE_TLS", "--disable-tls"},
		argoParam{"reposerver.tls.minversion", "ARGOCD_TLS_MIN_VERSION", "--tlsminversion"}, true},
	{argoDex, argoParam{"dexserver.disable.tls", "ARGOCD_DEX_SERVER_DISABLE_TLS", "--disable-tls"}, argoParam{}, true},
}

// oldTLS are the TLS versions with known weaknesses.
var oldTLS = []string{"1.0", "1.1"}

// boolParam returns the value of a bool parameter of the containers of a
// component, and the problems: true when one container has true.
func boolParam(params map[string]string, tpl *kube.PodTemplate, p argoParam, def string) (bool, []string) {
	on := false
	var problems []string
	for _, c := range tpl.Containers {
		v, problem := effectiveParam(params, c, p, true, def)
		if problem != "" {
			problems = append(problems, problem)
			continue
		}
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			problems = append(problems, fmt.Sprintf("container %s has the value %q for %s, which is not true or false", c.Name, v, p.Key))
			continue
		}
		on = on || b
	}
	return on, problems
}

// checkClient returns the problems of a client of a component (to).
func checkClient(params map[string]string, tpl *kube.PodTemplate, c tlsClient, to, what string) []string {
	plain, p := boolParam(params, tpl, c.Plaintext, "false")
	if plain {
		p = append(p, fmt.Sprintf("%s connects to %s without TLS (%s): %s go as plain text", c.Workload, to, c.Plaintext.Key, what))
	}
	if c.HasStrict && !plain {
		strict, sp := boolParam(params, tpl, c.Strict, "false")
		p = append(p, sp...)
		if !strict {
			p = append(p, fmt.Sprintf("%s does not check the TLS certificate of %s (%s is not true): a pod in the network path can read %s", c.Workload, to, c.Strict.Key, what))
		}
	}
	return p
}

// checkArgoTLS returns the problems of the TLS settings of the components
// of Argo CD that run (templates), with the parameters (params) and
// argocd-cm (cm). Inside Argo CD, the clients of the repository server
// send the credentials of the repositories, and Dex gives the API server
// the identities of the users. The sign-in checks the identity provider
// with TLS: plainHTTPIssuers are the issuers that may use HTTP, for
// example a Keycloak that only 127.0.0.1 can reach.
func checkArgoTLS(params map[string]string, templates map[string]*kube.PodTemplate, cm map[string]string, plainHTTPIssuers []string) []string {
	var p []string
	if templates[argoRepoServer] != nil {
		for _, c := range repoServerClients {
			if tpl := templates[c.Workload]; tpl != nil {
				p = append(p, checkClient(params, tpl, c, "the repository server", "the credentials of the repositories")...)
			}
		}
	}
	if templates[argoDex] != nil && templates[argoServer] != nil {
		p = append(p, checkClient(params, templates[argoServer], dexClient, "Dex", "the identities of the users")...)
	}
	for _, s := range tlsServers {
		tpl := templates[s.Workload]
		if tpl == nil {
			continue
		}
		if s.HasDisable {
			off, dp := boolParam(params, tpl, s.Disable, "false")
			p = append(p, dp...)
			if off {
				p = append(p, fmt.Sprintf("%s serves without TLS (%s)", s.Workload, s.Disable.Key))
			}
		}
		if s.MinVersion.Key != "" {
			for _, c := range tpl.Containers {
				v, problem := effectiveParam(params, c, s.MinVersion, false, "1.2")
				if problem != "" {
					p = append(p, problem)
				}
				if slices.Contains(oldTLS, strings.TrimSpace(v)) {
					p = append(p, fmt.Sprintf("%s accepts TLS %s (%s), which has known weaknesses", s.Workload, strings.TrimSpace(v), s.MinVersion.Key))
				}
			}
		}
	}

	if on, err := strconv.ParseBool(strings.TrimSpace(cm["oidc.tls.insecure.skip.verify"])); err == nil && on {
		p = append(p, "argocd-cm has oidc.tls.insecure.skip.verify: Argo CD does not check the TLS certificate of the identity provider, thus a pod in the network path can give it other identities")
	}
	if raw := strings.TrimSpace(cm["oidc.config"]); raw != "" {
		var oidc struct {
			Issuer string `json:"issuer"`
		}
		if err := yaml.Unmarshal([]byte(raw), &oidc); err != nil {
			p = append(p, fmt.Sprintf("the guard cannot read oidc.config in argocd-cm (%s)", strings.ReplaceAll(err.Error(), ";", ",")))
		} else if strings.HasPrefix(strings.ToLower(oidc.Issuer), "http://") && !slices.Contains(plainHTTPIssuers, oidc.Issuer) {
			p = append(p, fmt.Sprintf("the issuer %s in oidc.config of argocd-cm has no TLS: the tokens and the client secret go as plain text", oidc.Issuer))
		}
	}
	return p
}
