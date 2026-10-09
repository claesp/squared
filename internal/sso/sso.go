// Package sso signs users in with an OpenID Connect provider, for example
// Keycloak. It uses the authorization code flow with PKCE, a state value
// and a nonce. It also verifies access tokens for the API.
package sso

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config is the configuration of the OpenID Connect client.
type Config struct {
	// Issuer is the issuer URL as it is in the tokens, for example
	// "https://id.example.com/realms/squared".
	Issuer string
	// DiscoveryURL is the base URL for discovery, if the server cannot use
	// the issuer URL, for example inside a cluster. It is optional. The
	// tokens must still have Issuer as issuer.
	DiscoveryURL string
	ClientID     string
	ClientSecret string
	RedirectURL  string // For example "https://squared.example.com/auth/callback".
	// PostLogoutURL is where the provider sends the browser after sign-out.
	PostLogoutURL string
	// GroupsClaim is the claim with the group names. Default "groups".
	GroupsClaim string
	// AdminGroup is the group of platform administrators. If it is empty,
	// the provider does not control the administrator flag.
	AdminGroup string
	// APIAudience enables OIDC access tokens for the API. Access tokens
	// must have this value in their "aud" claim.
	APIAudience string
}

// Identity is a verified user identity from the provider.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// Admin tells if the identity is in the administrator group. The second
// value is false if the provider does not control the administrator flag.
func (c Config) Admin(id *Identity) (admin, managed bool) {
	if c.AdminGroup == "" {
		return false, false
	}
	for _, g := range id.Groups {
		if strings.TrimPrefix(g, "/") == c.AdminGroup {
			return true, true
		}
	}
	return false, true
}

// Client is an OpenID Connect client. It does discovery on the first use
// and repeats it after a failure, thus the server can start before the
// provider is available.
type Client struct {
	cfg Config

	mu       sync.Mutex
	provider *provider
	failedAt time.Time
}

type provider struct {
	oauth       oauth2.Config
	idVerifier  *oidc.IDTokenVerifier
	apiVerifier *oidc.IDTokenVerifier
	endSession  string
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("sso: issuer, client ID and redirect URL are necessary")
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	return &Client{cfg: cfg}, nil
}

// Config returns the configuration.
func (c *Client) Config() Config { return c.cfg }

// IssuerOrigin returns the scheme and host of the issuer, for example
// "https://id.example.com". The content security policy needs it.
func (c *Client) IssuerOrigin() string {
	u, err := url.Parse(c.cfg.Issuer)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ErrUnavailable tells that discovery failed. The provider can be down.
var ErrUnavailable = errors.New("the identity provider is not available")

func (c *Client) get(ctx context.Context) (*provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, nil
	}
	// Do not ask the provider again for each request after a failure.
	if time.Since(c.failedAt) < 5*time.Second {
		return nil, ErrUnavailable
	}
	p, err := c.discover(ctx)
	if err != nil {
		c.failedAt = time.Now()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	c.provider = p
	return p, nil
}

func (c *Client) discover(ctx context.Context) (*provider, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	discovery := c.cfg.Issuer
	if c.cfg.DiscoveryURL != "" {
		// The discovery document then has endpoints for the browser
		// (authorization, end session) and for the server (token, keys).
		// The issuer in the tokens is still checked against Issuer.
		discovery = c.cfg.DiscoveryURL
		ctx = oidc.InsecureIssuerURLContext(ctx, c.cfg.Issuer)
	}
	p, err := oidc.NewProvider(ctx, discovery)
	if err != nil {
		return nil, err
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&extra); err != nil {
		return nil, err
	}
	out := &provider{
		oauth: oauth2.Config{
			ClientID:     c.cfg.ClientID,
			ClientSecret: c.cfg.ClientSecret,
			RedirectURL:  c.cfg.RedirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		// The verifier checks the signature, the issuer, the audience
		// (the client ID) and the expiry time.
		idVerifier: p.Verifier(&oidc.Config{ClientID: c.cfg.ClientID}),
		endSession: extra.EndSession,
	}
	if c.cfg.APIAudience != "" {
		out.apiVerifier = p.Verifier(&oidc.Config{ClientID: c.cfg.APIAudience})
	}
	return out, nil
}

// Login is the data of one sign-in attempt. The server keeps it until the
// provider sends the browser back.
type Login struct {
	State    string
	Nonce    string
	Verifier string // PKCE code verifier.
}

// NewLogin returns random values for a sign-in attempt.
func NewLogin() Login {
	return Login{State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier()}
}

// AuthURL returns the URL of the sign-in page of the provider.
func (c *Client) AuthURL(ctx context.Context, l Login) (string, error) {
	p, err := c.get(ctx)
	if err != nil {
		return "", err
	}
	return p.oauth.AuthCodeURL(l.State, oidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier)), nil
}

// Exchange changes the authorization code into tokens and returns the
// verified identity and the raw ID token. The caller must first check
// that the state in the callback is the state of l.
func (c *Client) Exchange(ctx context.Context, l Login, code string) (*Identity, string, error) {
	p, err := c.get(ctx)
	if err != nil {
		return nil, "", err
	}
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(l.Verifier))
	if err != nil {
		return nil, "", fmt.Errorf("exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, "", errors.New("the token response has no ID token")
	}
	idToken, err := p.idVerifier.Verify(ctx, raw)
	if err != nil {
		return nil, "", fmt.Errorf("verify ID token: %w", err)
	}
	if idToken.Nonce != l.Nonce {
		return nil, "", errors.New("the nonce of the ID token is not correct")
	}
	id, err := c.identity(idToken)
	if err != nil {
		return nil, "", err
	}
	return id, raw, nil
}

// VerifyAccessToken checks an access token for the API and returns the
// identity. It returns an error if API tokens are not enabled.
func (c *Client) VerifyAccessToken(ctx context.Context, raw string) (*Identity, error) {
	if c.cfg.APIAudience == "" {
		return nil, errors.New("OIDC access tokens are not enabled")
	}
	p, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := p.apiVerifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	return c.identity(tok)
}

func (c *Client) identity(tok *oidc.IDToken) (*Identity, error) {
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("read claims: %w", err)
	}
	id := &Identity{Issuer: tok.Issuer, Subject: tok.Subject}
	id.Email, _ = claims["email"].(string)
	id.EmailVerified, _ = claims["email_verified"].(bool)
	id.Name, _ = claims["name"].(string)
	if id.Name == "" {
		id.Name, _ = claims["preferred_username"].(string)
	}
	if groups, ok := claims[c.cfg.GroupsClaim].([]any); ok {
		for _, g := range groups {
			if s, ok := g.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	}
	if id.Subject == "" {
		return nil, errors.New("the token has no subject")
	}
	return id, nil
}

// LogoutURL returns the URL that ends the session at the provider, or ""
// if the provider has no end session endpoint.
func (c *Client) LogoutURL(ctx context.Context, idToken string) string {
	p, err := c.get(ctx)
	if err != nil || p.endSession == "" {
		return ""
	}
	q := url.Values{"client_id": {c.cfg.ClientID}}
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	if c.cfg.PostLogoutURL != "" {
		q.Set("post_logout_redirect_uri", c.cfg.PostLogoutURL)
	}
	sep := "?"
	if strings.Contains(p.endSession, "?") {
		sep = "&"
	}
	return p.endSession + sep + q.Encode()
}
