package guard

import (
	"fmt"
	"slices"
	"strings"
)

// forgejoMirror is one row of the view squared_guard_mirrors.
type forgejoMirror struct {
	Owner      string
	Repo       string
	Archived   bool
	Kind       string // "pull" or "push"
	RemoteHost string // The host of a push mirror.
}

// checkForgejoMirrors checks the mirrors of the repositories of the Squared
// organization. Squared is the only writer of these repositories (decision
// D1 of the architecture), and Argo CD deploys what they have. A pull
// mirror gets its content from another repository, thus Argo CD deploys
// what Squared did not write, and Forgejo refuses the pushes of Squared. A
// push mirror copies the GitOps content to another address. Archived
// repositories are not in the check: Forgejo does not update their
// mirrors. ignore are mirrors that an operator checked, as
// "<repository>/<pull or push>".
func checkForgejoMirrors(mirrors []forgejoMirror, org string, ignore []string) ([]string, error) {
	var problems []string
	for _, m := range mirrors {
		if !strings.EqualFold(m.Owner, org) || m.Archived || slices.Contains(ignore, m.Repo+"/"+m.Kind) {
			continue
		}
		switch m.Kind {
		case "pull":
			problems = append(problems, fmt.Sprintf("%s/%s is a pull mirror: Forgejo replaces its content with the content of another repository", m.Owner, m.Repo))
		case "push":
			problems = append(problems, fmt.Sprintf("%s/%s has a push mirror to %s: Forgejo copies its content there", m.Owner, m.Repo, m.RemoteHost))
		default:
			problems = append(problems, fmt.Sprintf("%s/%s has a mirror of the unknown kind %s", m.Owner, m.Repo, m.Kind))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("mirrors connect the GitOps repositories with other repositories: %s. Remove the push mirrors, and make pull mirrors normal repositories. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return nil, nil
}
