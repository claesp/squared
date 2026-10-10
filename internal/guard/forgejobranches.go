package guard

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// forgejoBranchRule is one row of the view squared_guard_branch_rules: the
// protection rule of the default branch of a repository.
type forgejoBranchRule struct {
	Owner                string
	Repo                 string
	Branch               string
	RepoCreated          time.Time
	Archived             bool
	Protected            bool // A rule with the exact name of the branch exists.
	CanPush              bool
	EnableWhitelist      bool
	PushUsers            []string
	PushTeams            int
	PushDeployKeys       bool
	EnableMergeWhitelist bool
	MergeUsers           []string
	MergeTeams           int
	ApplyToAdmins        bool
}

// forgejoNewRepoTime is the time that the worker has to protect a new
// repository. The worker protects it in the same task that makes it.
const forgejoNewRepoTime = 5 * time.Minute

// checkForgejoBranches checks the protection of the default branch of the
// repositories of the Squared organization. Squared is the only writer of
// these repositories (decision D1 of the architecture), and Argo CD deploys
// what they have. The rule is a second line after the access checks of
// forgejo-writers: it also stops a Forgejo administrator, who has write
// access to all repositories, and a user who gets write access by mistake.
// Forgejo refuses all force pushes to a protected branch and all pushes to
// an archived repository. writers are the users that may push, ignore are
// repositories that an operator checked.
func checkForgejoBranches(rules []forgejoBranchRule, org string, writers, ignore []string, now time.Time) ([]string, error) {
	var warnings, problems []string
	for _, r := range rules {
		if !strings.EqualFold(r.Owner, org) || r.Archived || slices.Contains(ignore, r.Repo) {
			continue
		}
		where := fmt.Sprintf("branch %s of %s/%s", r.Branch, r.Owner, r.Repo)
		if !r.Protected {
			if now.Sub(r.RepoCreated) < forgejoNewRepoTime {
				warnings = append(warnings, "Forgejo "+where+" is new and has no protection rule yet")
				continue
			}
			problems = append(problems, where+" has no protection rule with the name of the branch: everybody with write access, also a Forgejo administrator, can push")
			continue
		}
		var weak []string
		switch {
		case !r.CanPush:
			weak = append(weak, "nobody can push, also not Squared")
		case !r.EnableWhitelist:
			weak = append(weak, "everybody with write access can push")
		default:
			weak = append(weak, listProblems("push", r.PushUsers, r.PushTeams, writers)...)
			if r.PushDeployKeys {
				weak = append(weak, "deploy keys with write access can push")
			}
		}
		if !r.EnableMergeWhitelist {
			weak = append(weak, "everybody with write access can merge pull requests")
		} else {
			weak = append(weak, listProblems("merge", r.MergeUsers, r.MergeTeams, writers)...)
		}
		if !r.ApplyToAdmins {
			weak = append(weak, "the rule does not apply to Forgejo administrators")
		}
		if len(weak) > 0 {
			problems = append(problems, where+": "+strings.Join(weak, ", "))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the branch protection of the GitOps repositories is missing or weak: %s. Squared corrects its rules at the next sync of the tenant (the platform repository at the start of the worker); remove other changes. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

// listProblems compares the users and teams that can push or merge with
// the writers.
func listProblems(action string, users []string, teams int, writers []string) []string {
	var p []string
	var others []string
	for _, u := range users {
		if !slices.Contains(writers, u) {
			others = append(others, u)
		}
	}
	if len(others) > 0 {
		p = append(p, fmt.Sprintf("the users %s can %s", strings.Join(others, ", "), action))
	}
	if teams > 0 {
		p = append(p, fmt.Sprintf("%d teams can %s", teams, action))
	}
	for _, w := range writers {
		if !slices.Contains(users, w) {
			p = append(p, fmt.Sprintf("Squared (%s) cannot %s", w, action))
		}
	}
	return p
}
