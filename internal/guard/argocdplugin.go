package guard

import (
	"cmp"
	"fmt"
	"net/url"
	"strings"
)

// pluginTokenPrefix is the start of the key of a plugin token. Argo CD
// reads the token from the key in argocd-secret ("$<key>") or in another
// Secret of its namespace ("$<secret>:<key>"), without more checks. Thus
// a plugin can get each Secret of the namespace, for example the client
// secret of Keycloak or the credentials of a repository. Only keys that
// start with this text are plugin tokens.
const pluginTokenPrefix = "plugin."

// inCluster tells if a host is a Service in the cluster. A full stop at
// the end makes a fully qualified name.
func inCluster(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

// checkArgoPlugin returns the problems of the ConfigMap of a plugin
// generator. The ApplicationSet controller reads it in its own namespace,
// and sends the token to the address in baseUrl with each request.
func checkArgoPlugin(data map[string]string) []string {
	var p []string
	token := strings.TrimSpace(data["token"])
	switch {
	case token == "":
	case !strings.HasPrefix(token, "$"):
		p = append(p, "it has the token as text, which each person who can read the ConfigMaps of the namespace sees")
	default:
		secret, key, ok := strings.Cut(token[1:], ":")
		if !ok {
			secret, key = "argocd-secret", token[1:]
		}
		if !strings.HasPrefix(key, pluginTokenPrefix) {
			p = append(p, fmt.Sprintf("its token is the key %s of the Secret %s, which does not start with %s: a plugin can get each Secret of the namespace, for example the client secret of Keycloak or repository credentials",
				key, secret, pluginTokenPrefix))
		}
	}

	base := strings.TrimSpace(data["baseUrl"])
	u, err := url.Parse(base)
	switch {
	case base == "":
		p = append(p, "it has no baseUrl")
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		p = append(p, fmt.Sprintf("its baseUrl %q is not an http or https address", base))
	case u.Scheme == "http" && token != "" && !inCluster(u.Hostname()):
		p = append(p, fmt.Sprintf("it sends its token to %s without TLS", base))
	}
	return p
}

// checkArgoDecision returns the problems and the warnings of the ConfigMap
// of a cluster decision resource generator. The generator reads the
// objects of the kind in the ConfigMap in the namespace of Argo CD. Each
// entry of the list statusListKey in their status whose matchKey is the
// name of a cluster of Argo CD gives parameters: all fields of the entry,
// and the name and the address of the cluster.
//
// The status of an object of Argo CD has data from the manifests of the
// tenants: the status of an application lists the objects in its
// manifests. Thus a tenant could give parameters to the ApplicationSet,
// with an object with the name of a cluster.
func checkArgoDecision(data map[string]string) (problems, warnings []string) {
	var missing []string
	for _, k := range []string{"apiVersion", "kind", "statusListKey", "matchKey"} {
		if strings.TrimSpace(data[k]) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		warnings = append(warnings, fmt.Sprintf("it has no %s, thus the generator makes no applications", strings.Join(missing, ", ")))
	}
	group, _, _ := strings.Cut(strings.TrimSpace(data["apiVersion"]), "/")
	if group == "argoproj.io" {
		problems = append(problems, fmt.Sprintf("it takes the decisions from the status of the Argo CD %s, which has data from the manifests of the tenants", cmp.Or(data["kind"], "objects")))
	}
	return problems, warnings
}
