package guard

import (
	"fmt"
	"slices"
	"strings"
)

// argoRepo is a repository or a credential template of Argo CD, as its
// API shows it: without passwords, tokens or keys.
type argoRepo struct {
	URL      string
	Template bool // A credential template: Argo CD uses it for all repositories whose address starts with URL.
	Write    bool // Argo CD can push with it (source hydrator).
	Insecure bool // Argo CD does not check the TLS certificate or the SSH host key.
}

func (r argoRepo) kind() string {
	kind := "repository"
	if r.Template {
		kind = "credential template"
	}
	if r.Write {
		kind = "write " + kind
	}
	return kind
}

// covers tells if the repository or template applies to repositories
// under the prefix: a repository under it, or a template whose address is
// the start of it or under it.
func (r argoRepo) covers(prefix string) bool {
	u, p := strings.TrimSuffix(r.URL, "/"), strings.TrimSuffix(prefix, "/")
	if strings.HasPrefix(u+"/", p+"/") {
		return true
	}
	return r.Template && strings.HasPrefix(p+"/", u+"/")
}

// endsInHost tells if a template address stops in the host name or the
// port, for example "https://git.example": Argo CD then also uses the
// credentials for "https://git.example.evil/".
func endsInHost(url string) bool {
	_, rest, ok := strings.Cut(url, "://")
	if !ok {
		// An address like "git@host:path" has the host before ":".
		_, path, ok := strings.Cut(url, ":")
		return !ok || path == ""
	}
	return !strings.Contains(rest, "/")
}

// checkArgoRepositories checks the repositories and credential templates
// of Argo CD. prefixes are the addresses of the GitOps repositories, for
// example "https://git.example/squared/". hydratorOn tells that Argo CD
// can push with write credentials.
func checkArgoRepositories(repos []argoRepo, prefixes []string, hydratorOn bool) ([]string, error) {
	var warnings, problems []string
	read := false
	for _, r := range repos {
		gitops := slices.ContainsFunc(prefixes, r.covers)
		if r.Template && endsInHost(r.URL) {
			problems = append(problems, fmt.Sprintf("the %s %s ends in the host name, thus Argo CD also sends its credentials to other hosts that start with it", r.kind(), r.URL))
		}
		switch {
		case r.Write && gitops:
			problems = append(problems, fmt.Sprintf("the %s %s lets Argo CD push to the GitOps repositories, which only Squared writes", r.kind(), r.URL))
		case r.Insecure && gitops:
			problems = append(problems, fmt.Sprintf("the %s %s does not check the TLS certificate or the SSH host key, thus a person in the network can change what Argo CD deploys", r.kind(), r.URL))
		case r.Insecure:
			warnings = append(warnings, fmt.Sprintf("the %s %s does not check the TLS certificate or the SSH host key", r.kind(), r.URL))
		}
		if gitops && !r.Write {
			read = true
		}
	}
	if !read {
		problems = append(problems, "the guard sees no repository or credential template of Argo CD for the GitOps repositories: Argo CD cannot read them, or the account of the guard has no right \"repositories, get\"")
	}
	if hydratorOn {
		warnings = append(warnings, "the source hydrator of Argo CD is on: Argo CD can push with write credentials. The API of Argo CD 3.5 shows only the write templates that have the address of a read template, thus the guard can miss one")
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the repositories of Argo CD are not safe: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
