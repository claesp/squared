package guard

import (
	"fmt"
	"slices"
	"strings"
)

// argoCluster is a cluster of Argo CD, as its API shows it: without the
// token, the keys and the settings of an exec provider.
type argoCluster struct {
	Server   string
	Name     string
	Project  string
	Insecure bool // Argo CD does not check the TLS certificate of the API server.
}

// argoInCluster is the address of the cluster in which Argo CD runs. Argo
// CD uses its own service account for it, and shows it with the name
// "in-cluster", unless a cluster Secret has this address.
const argoInCluster = "https://kubernetes.default.svc"

// checkArgoClusters checks the clusters of Argo CD. servers are the
// clusters that Squared deploys to (SQUARED_CLUSTER_SERVER). Argo CD sends
// the token of a cluster with each request and applies the manifests of
// the tenants there, thus a request without the check of the certificate
// lets a person in the network take the token or change the workloads.
func checkArgoClusters(clusters []argoCluster, servers []string) ([]string, error) {
	var warnings, problems []string
	seen := map[string]bool{}
	for _, c := range clusters {
		squared := slices.Contains(servers, c.Server)
		seen[c.Server] = true
		var weak []string
		if c.Insecure {
			weak = append(weak, "does not check the TLS certificate of the API server")
		}
		if strings.HasPrefix(strings.ToLower(c.Server), "http://") {
			weak = append(weak, "connects without TLS")
		}
		for _, w := range weak {
			msg := fmt.Sprintf("the cluster %s (%s) %s, thus a person in the network can take its token or change what Argo CD applies", c.Server, c.Name, w)
			if squared {
				problems = append(problems, msg)
			} else {
				warnings = append(warnings, msg)
			}
		}
		if c.Server == argoInCluster && c.Name != "in-cluster" {
			warnings = append(warnings, fmt.Sprintf("Argo CD uses the credentials of the cluster Secret %q for %s, not its own service account; check the rights of these credentials", c.Name, c.Server))
		}
	}
	for _, s := range servers {
		if !seen[s] {
			problems = append(problems, fmt.Sprintf("the guard does not see the cluster %s, to which Squared deploys: Argo CD has no such cluster, or the account of the guard has no right \"clusters, get\"", s))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the clusters of Argo CD are not safe: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
