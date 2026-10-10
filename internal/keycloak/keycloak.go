// Package keycloak is a client for the parts of the Keycloak admin API
// that the worker uses: groups and group members. It signs in with the
// client credentials grant of a service account.
package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"squared/internal/rest"
)

// ErrUserNotFound tells that Keycloak has no user with the email address.
var ErrUserNotFound = errors.New("keycloak: user not found")

// Client talks to one realm.
type Client struct {
	baseURL      string // For example "https://id.example.com".
	realm        string
	clientID     string
	clientSecret string
	http         *http.Client
	rest         rest.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New returns a Client.
func New(baseURL, realm, clientID, clientSecret string, httpClient *http.Client) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		realm:        realm,
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         httpClient,
	}
	if c.http == nil {
		c.http = rest.NewHTTPClient(nil)
	}
	c.rest = rest.Client{
		BaseURL: c.baseURL + "/admin/realms/" + url.PathEscape(realm),
		HTTP:    c.http,
		Auth: func(r *http.Request) error {
			token, err := c.accessToken(r.Context())
			if err != nil {
				return err
			}
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	}
	return c
}

// accessToken returns a valid access token of the service account. It
// gets a new token 30 seconds before the old token expires.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > 30*time.Second {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	tokenURL := c.baseURL + "/realms/" + url.PathEscape(c.realm) + "/protocol/openid-connect/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(c.clientID), url.QueryEscape(c.clientSecret))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("keycloak token: no access token in the answer")
	}
	c.token, c.expires = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return c.token, nil
}

// Group is a Keycloak group.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindGroup returns the top-level group with a name, or nil.
func (c *Client) FindGroup(ctx context.Context, name string) (*Group, error) {
	var groups []Group
	path := "/groups?exact=true&briefRepresentation=true&search=" + url.QueryEscape(name)
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &groups); err != nil {
		return nil, fmt.Errorf("find group %s: %w", name, err)
	}
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], nil
		}
	}
	return nil, nil
}

// EnsureGroup makes a top-level group if it does not exist and returns it.
func (c *Client) EnsureGroup(ctx context.Context, name string) (*Group, error) {
	g, err := c.FindGroup(ctx, name)
	if err != nil || g != nil {
		return g, err
	}
	err = c.rest.Do(ctx, http.MethodPost, "/groups", map[string]string{"name": name}, nil)
	if err != nil && !rest.HasStatus(err, http.StatusConflict) {
		return nil, fmt.Errorf("make group %s: %w", name, err)
	}
	g, err = c.FindGroup(ctx, name)
	if err == nil && g == nil {
		err = fmt.Errorf("group %s is missing after it was made", name)
	}
	return g, err
}

// DeleteGroup removes a top-level group. It does nothing if the group
// does not exist.
func (c *Client) DeleteGroup(ctx context.Context, name string) error {
	g, err := c.FindGroup(ctx, name)
	if err != nil || g == nil {
		return err
	}
	err = c.rest.Do(ctx, http.MethodDelete, "/groups/"+url.PathEscape(g.ID), nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// Members returns the IDs of the direct members of a group.
func (c *Client) Members(ctx context.Context, groupID string) ([]string, error) {
	var ids []string
	for first := 0; ; first += 500 {
		var users []struct {
			ID string `json:"id"`
		}
		path := fmt.Sprintf("/groups/%s/members?briefRepresentation=true&first=%d&max=500", url.PathEscape(groupID), first)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &users); err != nil {
			return nil, fmt.Errorf("members of group %s: %w", groupID, err)
		}
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		if len(users) < 500 {
			return ids, nil
		}
	}
}

// AddMember adds a user to a group.
func (c *Client) AddMember(ctx context.Context, groupID, userID string) error {
	return c.rest.Do(ctx, http.MethodPut, "/users/"+url.PathEscape(userID)+"/groups/"+url.PathEscape(groupID), nil, nil)
}

// RemoveMember removes a user from a group.
func (c *Client) RemoveMember(ctx context.Context, groupID, userID string) error {
	err := c.rest.Do(ctx, http.MethodDelete, "/users/"+url.PathEscape(userID)+"/groups/"+url.PathEscape(groupID), nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// UserIDByEmail returns the ID of the user with an email address.
func (c *Client) UserIDByEmail(ctx context.Context, email string) (string, error) {
	var users []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	path := "/users?exact=true&briefRepresentation=true&email=" + url.QueryEscape(email)
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &users); err != nil {
		return "", fmt.Errorf("find user %s: %w", email, err)
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return u.ID, nil
		}
	}
	return "", ErrUserNotFound
}
