// Package ssotest is a fake OpenID Connect provider for tests. It signs
// tokens with RS256, checks the client secret and PKCE, and has one user
// that the test can change.
package ssotest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// User is the identity that the provider signs in.
type User struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// Provider is a fake OpenID Connect provider.
type Provider struct {
	URL          string
	ClientID     string
	ClientSecret string

	key    *rsa.PrivateKey
	signer jose.Signer
	srv    *httptest.Server

	mu    sync.Mutex
	user  User
	codes map[string]grant
	// WrongNonce makes the provider put a wrong nonce in ID tokens.
	WrongNonce bool
}

type grant struct {
	nonce, challenge, redirect string
	user                       User
}

// New starts a provider. It stops when the test ends.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{ClientID: "squared", ClientSecret: "client-secret", key: key, signer: signer, codes: map[string]grant{},
		user: User{Subject: "user-1", Email: "user@example.com", EmailVerified: true, Name: "Test User"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.keys)
	mux.HandleFunc("GET /auth", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	p.srv = httptest.NewServer(mux)
	p.URL = p.srv.URL
	t.Cleanup(p.srv.Close)
	return p
}

// Close stops the provider.
func (p *Provider) Close() { p.srv.Close() }

// SetUser changes the user that the provider signs in.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	p.user = u
	p.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/auth",
		"token_endpoint":                        p.URL + "/token",
		"jwks_uri":                              p.URL + "/keys",
		"end_session_endpoint":                  p.URL + "/logout",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *Provider) keys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

// authorize signs the user in without a form and sends the browser back.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := rand.Text()
	p.mu.Lock()
	p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), user: p.user}
	p.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code")) // A code is used one time.
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.PostForm.Get("redirect_uri") != g.redirect {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	nonce := g.nonce
	if p.WrongNonce {
		nonce = "wrong"
	}
	writeJSON(w, map[string]any{
		"access_token": p.sign(g.user, "squared-api", ""),
		"id_token":     p.sign(g.user, p.ClientID, nonce),
		"token_type":   "Bearer",
		"expires_in":   300,
	})
}

// AccessToken returns a signed access token for the current user with an
// audience.
func (p *Provider) AccessToken(audience string) string {
	p.mu.Lock()
	u := p.user
	p.mu.Unlock()
	return p.sign(u, audience, "")
}

func (p *Provider) sign(u User, audience, nonce string) string {
	now := time.Now()
	claims := map[string]any{
		"iss":            p.URL,
		"sub":            u.Subject,
		"aud":            audience,
		"iat":            now.Unix(),
		"exp":            now.Add(5 * time.Minute).Unix(),
		"email":          u.Email,
		"email_verified": u.EmailVerified,
		"name":           u.Name,
		"groups":         u.Groups,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	raw, err := jwt.Signed(p.signer).Claims(claims).Serialize()
	if err != nil {
		panic(err) // Only possible with a bad key.
	}
	return raw
}
