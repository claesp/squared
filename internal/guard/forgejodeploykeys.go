package guard

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// forgejoDeployKey is one row of the view squared_guard_deploy_keys.
type forgejoDeployKey struct {
	Owner    string // Owner of the repository, for example the organization "squared".
	Repo     string
	Name     string
	Mode     int // 1: read, 2: write (Forgejo access modes).
	Created  time.Time
	LastUsed time.Time
}

// forgejoAccessWrite is the access mode "write" of Forgejo.
const forgejoAccessWrite = 2

// checkForgejoDeployKeys checks the deploy keys of the repositories of the
// Squared organization. Squared is the only writer of these repositories
// (decision D1 of the architecture), and Argo CD deploys what they have. A
// deploy key with write access pushes without Squared, without a user and
// without Keycloak, also after the person who added it left. A key with
// read access reads all manifests of the repository. The guard checks the
// keys also when SSH is off: they work when somebody turns SSH on. ignore
// are keys that an operator checked, as "<repository>/<key name>".
func checkForgejoDeployKeys(keys []forgejoDeployKey, org string, ignore []string) ([]string, error) {
	var warnings, problems []string
	for _, k := range keys {
		if !strings.EqualFold(k.Owner, org) || slices.Contains(ignore, k.Repo+"/"+k.Name) {
			continue
		}
		where := fmt.Sprintf("deploy key %q of %s/%s (made %s, last use %s)", k.Name, k.Owner, k.Repo,
			k.Created.UTC().Format(time.DateOnly), k.LastUsed.UTC().Format(time.DateOnly))
		if k.Mode >= forgejoAccessWrite {
			problems = append(problems, where+" can push")
		} else {
			warnings = append(warnings, "Forgejo "+where+" can read the repository; remove it, or add it to ignoreFields")
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("deploy keys can write to the GitOps repositories without Squared: %s. Remove the keys. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
