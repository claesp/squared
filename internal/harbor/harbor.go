// Package harbor is a client for the parts of the Harbor API v2.0 that the
// worker uses: projects, robot accounts and artifacts.
package harbor

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"squared/internal/rest"
)

// ErrImageNotFound tells that an image or tag is not in the registry.
var ErrImageNotFound = errors.New("image not found in the registry")

// Client talks to Harbor with a user name and password.
type Client struct {
	rest rest.Client
}

// New returns a Client. baseURL is the root URL of Harbor, for example
// "http://harbor-core".
func New(baseURL, user, password string, httpClient *http.Client) *Client {
	return &Client{rest: rest.Client{
		BaseURL: strings.TrimRight(baseURL, "/") + "/api/v2.0",
		HTTP:    httpClient,
		Auth: func(r *http.Request) error {
			r.SetBasicAuth(user, password)
			// Harbor needs this header for requests with basic
			// authentication that change data.
			r.Header.Set("X-Requested-With", "squared")
			return nil
		},
	}}
}

// Project is a Harbor project.
type Project struct {
	ID   int64  `json:"project_id"`
	Name string `json:"name"`
}

// EnsureProject makes a project if it does not exist and returns it.
func (c *Client) EnsureProject(ctx context.Context, name string, public bool) (*Project, error) {
	p, err := c.project(ctx, name)
	if err == nil {
		return p, nil
	}
	if !rest.HasStatus(err, http.StatusNotFound) {
		return nil, err
	}
	in := map[string]any{
		"project_name":  name,
		"metadata":      map[string]string{"public": fmt.Sprint(public)},
		"storage_limit": -1,
	}
	err = c.rest.Do(ctx, http.MethodPost, "/projects", in, nil)
	if err != nil && !rest.HasStatus(err, http.StatusConflict) {
		return nil, fmt.Errorf("create project %s: %w", name, err)
	}
	return c.project(ctx, name)
}

func (c *Client) project(ctx context.Context, name string) (*Project, error) {
	var p Project
	if err := c.rest.Do(ctx, http.MethodGet, "/projects/"+url.PathEscape(name), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Robot is a robot account with its secret.
type Robot struct {
	ID     int64
	Name   string // Full name, for example "robot$acme+pull".
	Secret string
}

type robotInfo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PullRobot makes or updates the robot account "<project>+<name>" that can
// pull images from a project. It always sets a new secret and returns it,
// because Harbor shows a secret only when it is made.
func (c *Client) PullRobot(ctx context.Context, project *Project, name string) (*Robot, error) {
	secret, err := newSecret()
	if err != nil {
		return nil, err
	}
	q := url.QueryEscape(fmt.Sprintf("Level=project,ProjectID=%d", project.ID))
	var robots []robotInfo
	if err := c.rest.Do(ctx, http.MethodGet, "/robots?page_size=100&q="+q, nil, &robots); err != nil {
		return nil, fmt.Errorf("list robots of %s: %w", project.Name, err)
	}
	fullName := "robot$" + project.Name + "+" + name
	for _, r := range robots {
		if r.Name != fullName {
			continue
		}
		in := map[string]string{"secret": secret}
		if err := c.rest.Do(ctx, http.MethodPatch, fmt.Sprintf("/robots/%d", r.ID), in, nil); err != nil {
			return nil, fmt.Errorf("refresh robot %s: %w", fullName, err)
		}
		return &Robot{ID: r.ID, Name: fullName, Secret: secret}, nil
	}
	in := map[string]any{
		"name":        name,
		"description": "Squared: image pulls for Kubernetes",
		"level":       "project",
		"duration":    -1,
		"secret":      secret,
		"permissions": []map[string]any{{
			"kind":      "project",
			"namespace": project.Name,
			"access":    []map[string]string{{"resource": "repository", "action": "pull"}},
		}},
	}
	var out struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if err := c.rest.Do(ctx, http.MethodPost, "/robots", in, &out); err != nil {
		return nil, fmt.Errorf("create robot %s: %w", fullName, err)
	}
	if out.Secret == "" {
		out.Secret = secret
	}
	return &Robot{ID: out.ID, Name: out.Name, Secret: out.Secret}, nil
}

// newSecret returns a random robot secret that agrees with the Harbor
// rules: 8 to 128 characters with at least one upper case letter, one
// lower case letter and one digit.
func newSecret() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 40)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		b[i] = chars[n.Int64()]
	}
	// Make sure that each class is present.
	b[0], b[1], b[2] = 'a', 'Z', '7'
	return string(b), nil
}

// Digest returns the digest of an image tag or digest in a project.
func (c *Client) Digest(ctx context.Context, project, repository, reference string) (string, error) {
	// Harbor needs the repository name encoded two times, because it can
	// contain "/".
	repo := url.PathEscape(url.PathEscape(repository))
	path := fmt.Sprintf("/projects/%s/repositories/%s/artifacts/%s", url.PathEscape(project), repo, url.PathEscape(reference))
	var out struct {
		Digest string `json:"digest"`
	}
	err := c.rest.Do(ctx, http.MethodGet, path, nil, &out)
	if rest.HasStatus(err, http.StatusNotFound) {
		return "", fmt.Errorf("%w: %s/%s:%s", ErrImageNotFound, project, repository, reference)
	}
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Digest, "sha256:") {
		return "", fmt.Errorf("artifact %s/%s:%s has no sha256 digest", project, repository, reference)
	}
	return out.Digest, nil
}

// Ping checks the connection and the credentials.
func (c *Client) Ping(ctx context.Context) error {
	return c.rest.Do(ctx, http.MethodGet, "/users/current", nil, nil)
}

// Harbor project roles.
const (
	RoleProjectAdmin = 1
	RoleDeveloper    = 2 // Push and pull.
	RoleGuest        = 3 // Pull.
	RoleMaintainer   = 4 // Push, pull and delete, but no member changes.
)

// groupTypeOIDC is the Harbor type of a group that comes from the groups
// claim of the OIDC provider.
const groupTypeOIDC = 3

// Member is a member of a project.
type Member struct {
	ID         int64  `json:"id"`
	EntityName string `json:"entity_name"`
	EntityType string `json:"entity_type"` // "u" for a user, "g" for a group.
	RoleID     int    `json:"role_id"`
}

// Members returns the members of a project.
func (c *Client) Members(ctx context.Context, project string) ([]Member, error) {
	var all []Member
	for page := 1; ; page++ {
		var members []Member
		path := fmt.Sprintf("/projects/%s/members?page=%d&page_size=100", url.PathEscape(project), page)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &members); err != nil {
			return nil, fmt.Errorf("members of project %s: %w", project, err)
		}
		all = append(all, members...)
		if len(members) < 100 {
			return all, nil
		}
	}
}

// AddGroupMember gives an OIDC group a role in a project. The group does
// not need to exist in Harbor before.
func (c *Client) AddGroupMember(ctx context.Context, project, group string, role int) error {
	in := map[string]any{
		"role_id":      role,
		"member_group": map[string]any{"group_name": group, "group_type": groupTypeOIDC},
	}
	err := c.rest.Do(ctx, http.MethodPost, "/projects/"+url.PathEscape(project)+"/members", in, nil)
	if err != nil {
		return fmt.Errorf("add group %s to project %s: %w", group, project, err)
	}
	return nil
}

// SetMemberRole changes the role of a member.
func (c *Client) SetMemberRole(ctx context.Context, project string, memberID int64, role int) error {
	path := fmt.Sprintf("/projects/%s/members/%d", url.PathEscape(project), memberID)
	return c.rest.Do(ctx, http.MethodPut, path, map[string]int{"role_id": role}, nil)
}

// RemoveMember removes a member from a project.
func (c *Client) RemoveMember(ctx context.Context, project string, memberID int64) error {
	path := fmt.Sprintf("/projects/%s/members/%d", url.PathEscape(project), memberID)
	err := c.rest.Do(ctx, http.MethodDelete, path, nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
