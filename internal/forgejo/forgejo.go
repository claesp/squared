// Package forgejo is a client for the parts of the Forgejo API that the
// worker uses: organizations, repositories and file changes.
package forgejo

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"squared/internal/rest"
)

// Client talks to Forgejo with an access token.
type Client struct {
	rest rest.Client
	// Author is the name and email address in commits.
	AuthorName  string
	AuthorEmail string
}

// New returns a Client. baseURL is the root URL of Forgejo, for example
// "http://forgejo:3000".
func New(baseURL, token string, httpClient *http.Client) *Client {
	return &Client{
		rest: rest.Client{
			BaseURL: strings.TrimRight(baseURL, "/") + "/api/v1",
			HTTP:    httpClient,
			Auth: func(r *http.Request) error {
				r.Header.Set("Authorization", "token "+token)
				return nil
			},
		},
		AuthorName:  "Squared",
		AuthorEmail: "squared@localhost",
	}
}

// EnsureOrg makes a private organization if it does not exist.
func (c *Client) EnsureOrg(ctx context.Context, name string) error {
	err := c.rest.Do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(name), nil, nil)
	if err == nil {
		return nil
	}
	if !rest.HasStatus(err, http.StatusNotFound) {
		return err
	}
	in := map[string]any{"username": name, "visibility": "private"}
	err = c.rest.Do(ctx, http.MethodPost, "/orgs", in, nil)
	if rest.HasStatus(err, http.StatusUnprocessableEntity) || rest.HasStatus(err, http.StatusConflict) {
		return nil // Another worker made it.
	}
	return err
}

// EnsureRepo makes a private repository in an organization if it does not
// exist. The repository gets a first commit on the branch "main".
func (c *Client) EnsureRepo(ctx context.Context, org, name, description string) error {
	err := c.rest.Do(ctx, http.MethodGet, repoPath(org, name), nil, nil)
	if err == nil {
		return nil
	}
	if !rest.HasStatus(err, http.StatusNotFound) {
		return err
	}
	in := map[string]any{
		"name":           name,
		"description":    description,
		"private":        true,
		"auto_init":      true,
		"default_branch": "main",
		"readme":         "Default",
	}
	err = c.rest.Do(ctx, http.MethodPost, "/orgs/"+url.PathEscape(org)+"/repos", in, nil)
	if rest.HasStatus(err, http.StatusConflict) {
		return nil
	}
	return err
}

// ArchiveRepo makes a repository read-only. It does nothing if the
// repository does not exist.
func (c *Client) ArchiveRepo(ctx context.Context, org, name string) error {
	err := c.rest.Do(ctx, http.MethodPatch, repoPath(org, name), map[string]bool{"archived": true}, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func repoPath(org, repo string) string {
	return "/repos/" + url.PathEscape(org) + "/" + url.PathEscape(repo)
}

// Head returns the commit ID at the top of a branch.
func (c *Client) Head(ctx context.Context, org, repo, branch string) (string, error) {
	var out struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, repoPath(org, repo)+"/branches/"+url.PathEscape(branch), nil, &out); err != nil {
		return "", err
	}
	if out.Commit.ID == "" {
		return "", fmt.Errorf("branch %s of %s/%s has no commit", branch, org, repo)
	}
	return out.Commit.ID, nil
}

// Tree returns the path and blob SHA of each file at a commit.
func (c *Client) Tree(ctx context.Context, org, repo, commit string) (map[string]string, error) {
	files := map[string]string{}
	for page := 1; ; page++ {
		var out struct {
			Tree []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"tree"`
			Truncated  bool `json:"truncated"`
			Page       int  `json:"page"`
			TotalCount int  `json:"total_count"`
		}
		path := fmt.Sprintf("%s/git/trees/%s?recursive=true&per_page=1000&page=%d", repoPath(org, repo), url.PathEscape(commit), page)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		for _, e := range out.Tree {
			if e.Type == "blob" {
				files[e.Path] = e.SHA
			}
		}
		if !out.Truncated || len(out.Tree) == 0 {
			return files, nil
		}
		if page > 1000 {
			return nil, fmt.Errorf("tree of %s/%s is too large", org, repo)
		}
	}
}

// Change is one file operation in a commit.
type Change struct {
	Operation string // "create", "update" or "delete".
	Path      string
	Content   []byte
	SHA       string // The current blob SHA, for "update" and "delete".
}

// Commit applies changes in one commit on a branch and returns the new
// commit ID. Forgejo rejects the commit if a file changed after the
// caller read its SHA.
func (c *Client) Commit(ctx context.Context, org, repo, branch, message string, changes []Change) (string, error) {
	type file struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Content   string `json:"content,omitempty"`
		SHA       string `json:"sha,omitempty"`
	}
	type identity struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	files := make([]file, 0, len(changes))
	for _, ch := range changes {
		f := file{Operation: ch.Operation, Path: ch.Path, SHA: ch.SHA}
		if ch.Operation != "delete" {
			f.Content = base64.StdEncoding.EncodeToString(ch.Content)
		}
		files = append(files, f)
	}
	who := identity{Name: c.AuthorName, Email: c.AuthorEmail}
	in := map[string]any{
		"branch":    branch,
		"message":   message,
		"files":     files,
		"author":    who,
		"committer": who,
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.rest.Do(ctx, http.MethodPost, repoPath(org, repo)+"/contents", in, &out); err != nil {
		return "", fmt.Errorf("commit to %s/%s: %w", org, repo, err)
	}
	return out.Commit.SHA, nil
}

// BlobSHA returns the Git blob SHA-1 of content. Git uses SHA-1 to name
// objects; the use here is for comparison only, not for security.
func BlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Scope selects the paths that one sync owns.
type Scope func(path string) bool

// Prefix returns a scope with all paths under dir.
func Prefix(dir string) Scope {
	dir = strings.TrimSuffix(dir, "/") + "/"
	return func(p string) bool { return strings.HasPrefix(p, dir) }
}

// Sync makes the files in scope equal to desired, in one commit. It
// returns the commit ID that contains the desired files: a new commit, or
// the current head if no change was necessary. All paths in desired must
// be in scope.
func (c *Client) Sync(ctx context.Context, org, repo, branch string, scope Scope, desired map[string][]byte, message string) (commit string, changed bool, err error) {
	head, err := c.Head(ctx, org, repo, branch)
	if err != nil {
		return "", false, err
	}
	current, err := c.Tree(ctx, org, repo, head)
	if err != nil {
		return "", false, err
	}
	changes := Diff(current, scope, desired)
	if len(changes) == 0 {
		return head, false, nil
	}
	sha, err := c.Commit(ctx, org, repo, branch, message, changes)
	if err != nil {
		return "", false, err
	}
	return sha, true, nil
}

// Diff returns the changes that make the files in scope equal to desired.
// current maps paths to blob SHAs. The result is ordered by path.
func Diff(current map[string]string, scope Scope, desired map[string][]byte) []Change {
	var changes []Change
	for path, content := range desired {
		old, exists := current[path]
		switch {
		case !exists:
			changes = append(changes, Change{Operation: "create", Path: path, Content: content})
		case old != BlobSHA(content):
			changes = append(changes, Change{Operation: "update", Path: path, Content: content, SHA: old})
		}
	}
	for path, sha := range current {
		if _, keep := desired[path]; !keep && scope(path) {
			changes = append(changes, Change{Operation: "delete", Path: path, SHA: sha})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// Team is a team of an organization.
type Team struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// EnsureTeam makes a team with read access to the code of the repositories
// that the caller adds, if the team does not exist, and returns it.
func (c *Client) EnsureTeam(ctx context.Context, org, name, description string) (*Team, error) {
	if t, err := c.FindTeam(ctx, org, name); err != nil || t != nil {
		return t, err
	}
	in := map[string]any{
		"name":                      name,
		"description":               description,
		"permission":                "read",
		"units":                     []string{"repo.code"},
		"includes_all_repositories": false,
		"can_create_org_repo":       false,
	}
	var t Team
	if err := c.rest.Do(ctx, http.MethodPost, "/orgs/"+url.PathEscape(org)+"/teams", in, &t); err != nil {
		return nil, fmt.Errorf("make team %s: %w", name, err)
	}
	return &t, nil
}

// FindTeam returns the team with a name, or nil.
func (c *Client) FindTeam(ctx context.Context, org, name string) (*Team, error) {
	for page := 1; page < 100; page++ {
		var teams []Team
		path := fmt.Sprintf("/orgs/%s/teams?page=%d&limit=50", url.PathEscape(org), page)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &teams); err != nil {
			return nil, fmt.Errorf("list teams of %s: %w", org, err)
		}
		for i := range teams {
			if teams[i].Name == name {
				return &teams[i], nil
			}
		}
		if len(teams) < 50 {
			break
		}
	}
	return nil, nil
}

// DeleteTeam removes a team. It does nothing if the team does not exist.
func (c *Client) DeleteTeam(ctx context.Context, org, name string) error {
	t, err := c.FindTeam(ctx, org, name)
	if err != nil || t == nil {
		return err
	}
	err = c.rest.Do(ctx, http.MethodDelete, fmt.Sprintf("/teams/%d", t.ID), nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// AddTeamRepo gives a team access to a repository.
func (c *Client) AddTeamRepo(ctx context.Context, teamID int64, org, repo string) error {
	return c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s/%s", teamID, url.PathEscape(org), url.PathEscape(repo)), nil, nil)
}

// TeamMembers returns the user names of the members of a team.
func (c *Client) TeamMembers(ctx context.Context, teamID int64) ([]string, error) {
	var names []string
	for page := 1; page < 1000; page++ {
		var users []struct {
			Login string `json:"login"`
		}
		path := fmt.Sprintf("/teams/%d/members?page=%d&limit=50", teamID, page)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &users); err != nil {
			return nil, fmt.Errorf("members of team %d: %w", teamID, err)
		}
		for _, u := range users {
			names = append(names, u.Login)
		}
		if len(users) < 50 {
			break
		}
	}
	return names, nil
}

// AddTeamMember adds a user to a team.
func (c *Client) AddTeamMember(ctx context.Context, teamID int64, user string) error {
	return c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", teamID, url.PathEscape(user)), nil, nil)
}

// RemoveTeamMember removes a user from a team.
func (c *Client) RemoveTeamMember(ctx context.Context, teamID int64, user string) error {
	err := c.rest.Do(ctx, http.MethodDelete, fmt.Sprintf("/teams/%d/members/%s", teamID, url.PathEscape(user)), nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// UserNameFor returns the Forgejo user name for an email address: the part
// before "@", with characters that Forgejo does not accept changed to "-".
// Forgejo uses the same rule at the first sign-in (USERNAME = email).
func UserNameFor(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), ".-_")
	if name == "" {
		name = "user"
	}
	if len(name) > 30 {
		name = name[:30]
	}
	return name
}

// EnsureUser returns the name of the user with an email address. If no such
// user exists, it makes one with a random password that nobody knows. At
// the first sign-in with the identity provider, Forgejo links the account
// by the email address (ACCOUNT_LINKING = auto). The caller needs
// administrator rights.
func (c *Client) EnsureUser(ctx context.Context, email, fullName string) (string, error) {
	base := UserNameFor(email)
	for i := 1; i <= 20; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		var u struct {
			Email string `json:"email"`
		}
		err := c.rest.Do(ctx, http.MethodGet, "/users/"+url.PathEscape(name), nil, &u)
		if err == nil {
			if strings.EqualFold(u.Email, email) {
				return name, nil
			}
			continue // The name belongs to a different person.
		}
		if !rest.HasStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("find user %s: %w", name, err)
		}
		in := map[string]any{
			"username":             name,
			"email":                email,
			"full_name":            fullName,
			"password":             randomPassword(),
			"must_change_password": false,
			"send_notify":          false,
			"visibility":           "private",
		}
		err = c.rest.Do(ctx, http.MethodPost, "/admin/users", in, nil)
		if rest.HasStatus(err, http.StatusUnprocessableEntity) {
			continue // A reserved name, for example "admin". Try the next name.
		}
		if err != nil {
			return "", fmt.Errorf("make user %s: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("no free user name for %s", email)
}

func randomPassword() string {
	return "Sq-" + rand.Text() + rand.Text()
}
