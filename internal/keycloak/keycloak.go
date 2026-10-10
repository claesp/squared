// Package keycloak is a client for the parts of the Keycloak admin API
// that the worker and the guard use: groups, group members, admin
// permissions, and the mappers of clients. It signs in with the client
// credentials grant of a service account.
package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

// GroupByPath returns the group with a full path, for example
// "/squared-admins", or nil if it does not exist.
func (c *Client) GroupByPath(ctx context.Context, path string) (*Group, error) {
	var g Group
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	err := c.rest.Do(ctx, http.MethodGet, "/group-by-path/"+strings.Join(parts, "/"), nil, &g)
	if rest.HasStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find group %s: %w", path, err)
	}
	return &g, nil
}

// Permission is a scope permission of the admin-permissions client
// (fine-grained admin permissions, version 2).
type Permission struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	ResourceType     string   `json:"resourceType"`
	DecisionStrategy string   `json:"decisionStrategy,omitempty"`
	Resources        []string `json:"resources"` // IDs of the users, groups or clients.
	Scopes           []string `json:"scopes"`
	Policies         []string `json:"policies"` // Policy names.
}

func (c *Client) authzPath(ctx context.Context) (string, error) {
	var clients []struct {
		ID string `json:"id"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "/clients?clientId=admin-permissions", nil, &clients); err != nil {
		return "", fmt.Errorf("find admin-permissions client: %w", err)
	}
	if len(clients) != 1 {
		return "", errors.New("the realm has no admin-permissions client; turn on admin permissions")
	}
	return "/clients/" + url.PathEscape(clients[0].ID) + "/authz/resource-server", nil
}

// GetPermission returns the scope permission with a name.
func (c *Client) GetPermission(ctx context.Context, name string) (*Permission, error) {
	base, err := c.authzPath(ctx)
	if err != nil {
		return nil, err
	}
	var found []Permission
	if err := c.rest.Do(ctx, http.MethodGet, base+"/permission/scope?name="+url.QueryEscape(name), nil, &found); err != nil {
		return nil, fmt.Errorf("find permission %s: %w", name, err)
	}
	var p *Permission
	for i := range found {
		if found[i].Name == name {
			p = &found[i]
		}
	}
	if p == nil {
		return nil, fmt.Errorf("permission %s does not exist", name)
	}
	var full Permission
	if err := c.rest.Do(ctx, http.MethodGet, base+"/permission/scope/"+url.PathEscape(p.ID), nil, &full); err != nil {
		return nil, fmt.Errorf("read permission %s: %w", name, err)
	}
	p = &full
	type named struct {
		ID   string `json:"_id"`
		Name string `json:"name"`
	}
	var resources, scopes, policies []named
	if err := c.rest.Do(ctx, http.MethodGet, base+"/permission/"+url.PathEscape(p.ID)+"/resources", nil, &resources); err != nil {
		return nil, err
	}
	if err := c.rest.Do(ctx, http.MethodGet, base+"/permission/"+url.PathEscape(p.ID)+"/scopes", nil, &scopes); err != nil {
		return nil, err
	}
	if err := c.rest.Do(ctx, http.MethodGet, base+"/permission/"+url.PathEscape(p.ID)+"/associatedPolicies", nil, &policies); err != nil {
		return nil, err
	}
	p.Resources, p.Scopes, p.Policies = nil, nil, nil
	for _, r := range resources {
		p.Resources = append(p.Resources, r.Name) // The resource name is the ID of the group.
	}
	for _, s := range scopes {
		p.Scopes = append(p.Scopes, s.Name)
	}
	for _, s := range policies {
		p.Policies = append(p.Policies, s.Name)
	}
	return p, nil
}

// SetPermissionResources sets the resources of a scope permission and
// keeps its scopes and policies. It refuses an empty list, because a
// permission without resources applies to all resources of its type.
func (c *Client) SetPermissionResources(ctx context.Context, p *Permission, resources []string) error {
	if len(resources) == 0 {
		return errors.New("refused: a permission without resources applies to all resources")
	}
	base, err := c.authzPath(ctx)
	if err != nil {
		return err
	}
	update := *p
	update.Resources = resources
	return c.rest.Do(ctx, http.MethodPut, base+"/permission/scope/"+url.PathEscape(p.ID), update, nil)
}

// ProtocolMapper is a protocol mapper of a client or of a client scope. It
// puts a claim into the tokens.
type ProtocolMapper struct {
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"` // The type, for example "oidc-group-membership-mapper".
	Config         map[string]string `json:"config"`
}

// ClientMappers is the part of a client that tells which claims its tokens
// get: its own mappers and its client scopes. It has no secrets.
type ClientMappers struct {
	ClientID             string           `json:"clientId"`
	Protocol             string           `json:"protocol"`
	DefaultClientScopes  []string         `json:"defaultClientScopes"`
	OptionalClientScopes []string         `json:"optionalClientScopes"`
	ProtocolMappers      []ProtocolMapper `json:"protocolMappers"`
}

// ClientScope is a client scope with its mappers.
type ClientScope struct {
	Name            string           `json:"name"`
	Protocol        string           `json:"protocol"`
	ProtocolMappers []ProtocolMapper `json:"protocolMappers"`
}

// Clients returns the mappers and the client scopes of all clients. It
// needs the role view-clients. Keycloak removes the secrets from the
// answer unless the account can manage the client.
func (c *Client) Clients(ctx context.Context) ([]ClientMappers, error) {
	const page = 100
	var all []ClientMappers
	for first := 0; ; first += page {
		var clients []ClientMappers
		path := fmt.Sprintf("/clients?first=%d&max=%d", first, page)
		if err := c.rest.Do(ctx, http.MethodGet, path, nil, &clients); err != nil {
			return nil, fmt.Errorf("list clients: %w", err)
		}
		all = append(all, clients...)
		if len(clients) < page {
			return all, nil
		}
	}
}

// ClientScopes returns all client scopes with their mappers. It needs the
// role view-clients; without it, Keycloak returns an empty list.
func (c *Client) ClientScopes(ctx context.Context) ([]ClientScope, error) {
	var scopes []ClientScope
	if err := c.rest.Do(ctx, http.MethodGet, "/client-scopes", nil, &scopes); err != nil {
		return nil, fmt.Errorf("list client scopes: %w", err)
	}
	return scopes, nil
}

// RealmEmail are the realm settings that decide which email addresses the
// users can have.
type RealmEmail struct {
	DuplicateEmailsAllowed      bool
	RegistrationAllowed         bool
	RegistrationEmailAsUsername bool
	EditUsernameAllowed         bool
	VerifyEmail                 bool
	// EmailEditableByUser tells that the user profile permits users to
	// change their own address.
	EmailEditableByUser bool
	IdentityProviders   []IdentityProvider
	UserFederation      []UserFederation
}

// IdentityProvider is an identity provider of the realm (brokering).
type IdentityProvider struct {
	Alias      string
	Enabled    bool
	TrustEmail bool
}

// UserFederation is a user storage provider, for example LDAP.
type UserFederation struct {
	Name       string
	ProviderID string
	TrustEmail bool
}

// RealmEmailSettings reads the email settings of the realm. It needs
// view-realm (or manage-realm) and view-identity-providers. Keycloak
// removes the secrets of identity providers and user federation from the
// answers, and this function reads only the fields above.
func (c *Client) RealmEmailSettings(ctx context.Context) (*RealmEmail, error) {
	var realm struct {
		DuplicateEmailsAllowed      bool `json:"duplicateEmailsAllowed"`
		RegistrationAllowed         bool `json:"registrationAllowed"`
		RegistrationEmailAsUsername bool `json:"registrationEmailAsUsername"`
		EditUsernameAllowed         bool `json:"editUsernameAllowed"`
		VerifyEmail                 bool `json:"verifyEmail"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "", nil, &realm); err != nil {
		return nil, fmt.Errorf("read realm: %w", err)
	}
	out := &RealmEmail{
		DuplicateEmailsAllowed:      realm.DuplicateEmailsAllowed,
		RegistrationAllowed:         realm.RegistrationAllowed,
		RegistrationEmailAsUsername: realm.RegistrationEmailAsUsername,
		EditUsernameAllowed:         realm.EditUsernameAllowed,
		VerifyEmail:                 realm.VerifyEmail,
	}

	var profile struct {
		Attributes []struct {
			Name        string `json:"name"`
			Permissions struct {
				Edit []string `json:"edit"`
			} `json:"permissions"`
		} `json:"attributes"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "/users/profile", nil, &profile); err != nil {
		return nil, fmt.Errorf("read user profile: %w", err)
	}
	found := false
	for _, a := range profile.Attributes {
		if a.Name == "email" {
			found = true
			out.EmailEditableByUser = slices.Contains(a.Permissions.Edit, "user")
		}
	}
	if !found {
		return nil, errors.New("the user profile has no attribute email")
	}

	var idps []struct {
		Alias      string `json:"alias"`
		Enabled    bool   `json:"enabled"`
		TrustEmail bool   `json:"trustEmail"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "/identity-provider/instances?first=0&max=1000", nil, &idps); err != nil {
		return nil, fmt.Errorf("list identity providers: %w", err)
	}
	for _, p := range idps {
		out.IdentityProviders = append(out.IdentityProviders, IdentityProvider{Alias: p.Alias, Enabled: p.Enabled, TrustEmail: p.TrustEmail})
	}

	var feds []struct {
		Name       string `json:"name"`
		ProviderID string `json:"providerId"`
		Config     struct {
			TrustEmail []string `json:"trustEmail"`
		} `json:"config"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, "/components?type="+url.QueryEscape("org.keycloak.storage.UserStorageProvider"), nil, &feds); err != nil {
		return nil, fmt.Errorf("list user federation: %w", err)
	}
	for _, f := range feds {
		out.UserFederation = append(out.UserFederation, UserFederation{Name: f.Name, ProviderID: f.ProviderID,
			TrustEmail: slices.Contains(f.Config.TrustEmail, "true")})
	}
	return out, nil
}
