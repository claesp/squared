// Package forgejo is a client for the parts of the Forgejo API that the
// worker uses: organizations, repositories, file changes, teams and the
// accounts of team members.
package forgejo

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// BranchProtection is the part of a Forgejo branch protection rule that
// Squared sets.
type BranchProtection struct {
	RuleName                string   `json:"rule_name"`
	EnablePush              bool     `json:"enable_push"`
	EnablePushWhitelist     bool     `json:"enable_push_whitelist"`
	PushWhitelistUsernames  []string `json:"push_whitelist_usernames"`
	PushWhitelistTeams      []string `json:"push_whitelist_teams"`
	PushWhitelistDeployKeys bool     `json:"push_whitelist_deploy_keys"`
	EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
	MergeWhitelistTeams     []string `json:"merge_whitelist_teams"`
	ApplyToAdmins           bool     `json:"apply_to_admins"`
}

// ProtectBranch makes or corrects the protection rule of a branch, so that
// only the users in pushers can push to it or merge into it, also when the
// person is a Forgejo administrator. Forgejo refuses all force pushes to a
// protected branch. The rule has the exact name of the branch: Forgejo uses
// such a rule before a rule with a pattern.
func (c *Client) ProtectBranch(ctx context.Context, org, repo, branch string, pushers []string) error {
	want := BranchProtection{
		RuleName: branch, EnablePush: true, EnablePushWhitelist: true,
		PushWhitelistUsernames: slices.Sorted(slices.Values(pushers)), PushWhitelistTeams: []string{},
		EnableMergeWhitelist: true, MergeWhitelistUsernames: slices.Sorted(slices.Values(pushers)), MergeWhitelistTeams: []string{},
		ApplyToAdmins: true,
	}
	base := repoPath(org, repo) + "/branch_protections"
	var have BranchProtection
	err := c.rest.Do(ctx, http.MethodGet, base+"/"+url.PathEscape(branch), nil, &have)
	if rest.HasStatus(err, http.StatusNotFound) {
		in := map[string]any{"branch_name": branch}
		data, _ := json.Marshal(want)
		_ = json.Unmarshal(data, &in)
		if err := c.rest.Do(ctx, http.MethodPost, base, in, nil); err != nil {
			return fmt.Errorf("protect branch %s of %s/%s: %w", branch, org, repo, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the protection of branch %s of %s/%s: %w", branch, org, repo, err)
	}
	slices.Sort(have.PushWhitelistUsernames)
	slices.Sort(have.MergeWhitelistUsernames)
	if protectionEqual(have, want) {
		return nil
	}
	if err := c.rest.Do(ctx, http.MethodPatch, base+"/"+url.PathEscape(branch), want, nil); err != nil {
		return fmt.Errorf("correct the protection of branch %s of %s/%s: %w", branch, org, repo, err)
	}
	return nil
}

func protectionEqual(a, b BranchProtection) bool {
	return a.EnablePush == b.EnablePush && a.EnablePushWhitelist == b.EnablePushWhitelist &&
		slices.Equal(a.PushWhitelistUsernames, b.PushWhitelistUsernames) && len(a.PushWhitelistTeams) == 0 &&
		a.PushWhitelistDeployKeys == b.PushWhitelistDeployKeys && a.EnableMergeWhitelist == b.EnableMergeWhitelist &&
		slices.Equal(a.MergeWhitelistUsernames, b.MergeWhitelistUsernames) && len(a.MergeWhitelistTeams) == 0 &&
		a.ApplyToAdmins == b.ApplyToAdmins
}

// CurrentUser returns the user name of the token.
func (c *Client) CurrentUser(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", fmt.Errorf("read the user of the token: %w", err)
	}
	return u.Login, nil
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

// UserNameFor returns a Forgejo user name for a user name of the identity
// provider (or an email address: then the part before "@"), with
// characters that Forgejo does not accept changed to "-".
func UserNameFor(name string) string {
	local, _, _ := strings.Cut(strings.ToLower(name), "@")
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name = strings.Trim(b.String(), ".-_")
	if name == "" {
		name = "user"
	}
	if len(name) > 30 {
		name = name[:30]
	}
	return name
}

// LinkedUser is a Forgejo account that belongs to a user of an OAuth2
// authentication source, for example Keycloak.
type LinkedUser struct {
	SourceID int64  // ID of the authentication source in Forgejo.
	Subject  string // The subject (sub) of the user at the identity provider.
	Username string // Wanted user name; Forgejo gets UserNameFor(Username).
	Email    string
	FullName string
}

// ErrEmailInUse tells that Forgejo has an account with the email address
// that belongs to no user of the authentication source, or to another one.
var ErrEmailInUse = errors.New("forgejo: the email address belongs to another account")

// EnsureLinkedUser returns the name of the account of a user of an OAuth2
// authentication source. If the account does not exist, it makes one that
// belongs to the source and the subject, without a password. At the
// sign-in, Forgejo finds the account by the subject, never by the name or
// by the email address. The function never takes an existing account: if
// another account has the email address, it returns ErrEmailInUse. The
// caller needs administrator rights.
func (c *Client) EnsureLinkedUser(ctx context.Context, u LinkedUser) (string, error) {
	if u.SourceID <= 0 || u.Subject == "" {
		return "", errors.New("forgejo: a linked user needs a source ID and a subject")
	}
	var found []struct {
		Login     string `json:"login"`
		SourceID  int64  `json:"source_id"`
		LoginName string `json:"login_name"`
	}
	path := fmt.Sprintf("/admin/users?source_id=%d&login_name=%s&limit=50", u.SourceID, url.QueryEscape(u.Subject))
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &found); err != nil {
		return "", fmt.Errorf("find user %s: %w", u.Subject, err)
	}
	for _, f := range found {
		if f.SourceID == u.SourceID && f.LoginName == u.Subject {
			return f.Login, nil
		}
	}

	base := UserNameFor(u.Username)
	for i := 1; i <= 20; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		in := map[string]any{
			"username":             name,
			"email":                u.Email,
			"full_name":            u.FullName,
			"source_id":            u.SourceID,
			"login_name":           u.Subject,
			"must_change_password": false,
			"send_notify":          false,
			"visibility":           "private",
		}
		err := c.rest.Do(ctx, http.MethodPost, "/admin/users", in, nil)
		var se *rest.StatusError
		if errors.As(err, &se) && se.Code == http.StatusUnprocessableEntity {
			if strings.Contains(se.Body, "e-mail already in use") {
				return "", fmt.Errorf("%w: %s; an administrator must check that account", ErrEmailInUse, u.Email)
			}
			continue // The name is taken or reserved, for example "admin".
		}
		if err != nil {
			return "", fmt.Errorf("make user %s: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("no free user name for %s", u.Username)
}
