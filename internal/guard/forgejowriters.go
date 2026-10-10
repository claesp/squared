package guard

import (
	"fmt"
	"slices"
	"strings"
)

// forgejoRepoAccess is one row of the view squared_guard_repo_access: the
// access of a user to the code of repositories through a team or as a
// collaborator.
type forgejoRepoAccess struct {
	Org   string
	Kind  string // "team" or "collaborator"
	Via   string // The team, or the repository of a collaborator.
	User  string
	Mode  int    // 1: read, 2: write, 3: admin, 4: owner.
	Repos string // Repository names separated by commas, or "*" for all repositories.
}

// key is the name of the access in ignoreFields and in messages.
func (a forgejoRepoAccess) key() string { return a.Kind + ":" + a.Via + "/" + a.User }

func (a forgejoRepoAccess) description() string {
	repos := "all repositories"
	if a.Repos != "*" {
		repos = a.Repos
	}
	if a.Kind == "collaborator" {
		return fmt.Sprintf("user %s is a collaborator of %s/%s", a.User, a.Org, a.Via)
	}
	return fmt.Sprintf("user %s is in the team %s of %s (%s)", a.User, a.Via, a.Org, repos)
}

// checkForgejoWriters checks who can push to the repositories of the
// Squared organization. Squared is the only writer of these repositories
// (decision D1 of the architecture), and Argo CD deploys what they have.
// writers are the users that may push, for example the bot of the worker.
// A team or a collaboration that gives another user write access is an
// error. A collaboration with read access is a warning: the tenant teams
// give read access, and a collaborator reads outside the membership in
// Squared. ignore are accesses that an operator checked, as
// "team:<team>/<user>" or "collaborator:<repository>/<user>".
func checkForgejoWriters(access []forgejoRepoAccess, org string, writers, ignore []string) ([]string, error) {
	var warnings, problems []string
	for _, a := range access {
		if !strings.EqualFold(a.Org, org) || slices.Contains(writers, a.User) || slices.Contains(ignore, a.key()) {
			continue
		}
		switch {
		case a.Mode >= forgejoAccessWrite:
			problems = append(problems, a.description()+" and can push")
		case a.Kind == "collaborator" && a.Mode > 0:
			warnings = append(warnings, "Forgejo "+a.description()+" and can read it; remove the collaborator, or add "+a.key()+" to ignoreFields")
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("users other than Squared can write to the GitOps repositories: %s. Remove the access. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
