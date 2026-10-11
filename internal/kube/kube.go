// Package kube is a minimal Kubernetes API client. The worker uses it for
// two operations only: to write image pull secrets into tenant namespaces
// and to read the status of Argo CD applications. RBAC limits the service
// account of the worker to these operations.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"squared/internal/rest"
)

// Paths of the in-cluster service account files.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	fieldManager      = "squared"
)

// ErrNamespaceNotFound tells that a namespace does not exist yet.
var ErrNamespaceNotFound = errors.New("namespace not found")

// Config tells how to connect to the API server.
type Config struct {
	Server    string // For example "https://10.96.0.1:443".
	TokenFile string // Kubernetes rotates the token, thus the client reads the file again.
	CAFile    string
}

// InClusterConfig returns the configuration for a pod.
func InClusterConfig() (Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST is not set")
	}
	return Config{
		Server:    "https://" + net.JoinHostPort(host, port),
		TokenFile: serviceAccountDir + "/token",
		CAFile:    serviceAccountDir + "/ca.crt",
	}, nil
}

// Client is a Kubernetes API client.
type Client struct {
	rest rest.Client

	tokenFile string
	mu        sync.Mutex
	token     string
	readAt    time.Time
}

// New returns a Client.
func New(cfg Config) (*Client, error) {
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA file %s has no certificates", cfg.CAFile)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c := &Client{tokenFile: cfg.TokenFile}
	c.rest = rest.Client{
		BaseURL: strings.TrimRight(cfg.Server, "/"),
		HTTP:    rest.NewHTTPClient(transport),
		Auth: func(r *http.Request) error {
			token, err := c.bearer()
			if err != nil {
				return err
			}
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	}
	if _, err := c.bearer(); err != nil {
		return nil, err
	}
	return c, nil
}

// bearer returns the token. It reads the file again after one minute.
func (c *Client) bearer() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.readAt) < time.Minute {
		return c.token, nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	c.token, c.readAt = strings.TrimSpace(string(b)), time.Now()
	return c.token, nil
}

// SecretExists tells if a secret exists.
func (c *Client) SecretExists(ctx context.Context, namespace, name string) (bool, error) {
	err := c.rest.Do(ctx, http.MethodGet, secretPath(namespace, name), nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

func secretPath(namespace, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets/" + url.PathEscape(name)
}

// ApplyPullSecret makes or updates an image pull secret with server-side
// apply.
func (c *Client) ApplyPullSecret(ctx context.Context, namespace, name, registry, user, password string, labels map[string]string) error {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	config, err := json.Marshal(map[string]any{
		"auths": map[string]any{registry: map[string]string{"username": user, "password": password, "auth": auth}},
	})
	if err != nil {
		return err
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": labels},
		"type":       "kubernetes.io/dockerconfigjson",
		"data":       map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(config)},
	}
	path := secretPath(namespace, name) + "?fieldManager=" + fieldManager + "&force=true"
	err = c.rest.DoContent(ctx, http.MethodPatch, path, "application/apply-patch+yaml", secret, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %s", ErrNamespaceNotFound, namespace)
	}
	return err
}

// Application is the part of an Argo CD Application that Squared reads.
type Application struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Source struct {
			TargetRevision string `json:"targetRevision"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Health struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"health"`
		OperationState *struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"operationState"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// ListApplications returns the Argo CD applications in a namespace that
// have the labels in selector, for example "squared.dev/managed=true".
func (c *Client) ListApplications(ctx context.Context, namespace, selector string) ([]Application, error) {
	var out struct {
		Items []Application `json:"items"`
	}
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications?labelSelector=" + url.QueryEscape(selector)
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ListApplicationObjects returns all Argo CD applications in a namespace
// as complete objects, for the checks of the guard.
func (c *Client) ListApplicationObjects(ctx context.Context, namespace string) ([]json.RawMessage, error) {
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications"
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ApplicationExists tells if an Argo CD application exists.
func (c *Client) ApplicationExists(ctx context.Context, namespace, name string) (bool, error) {
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications/" + url.PathEscape(name)
	err := c.rest.Do(ctx, http.MethodGet, path, nil, nil)
	if rest.HasStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ConfigMap is the part of a ConfigMap that Squared reads.
type ConfigMap struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// GetConfigMap returns a ConfigMap.
func (c *Client) GetConfigMap(ctx context.Context, namespace, name string) (*ConfigMap, error) {
	var cm ConfigMap
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps/" + url.PathEscape(name)
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

// ListConfigMaps returns the ConfigMaps of a namespace that have the labels
// in selector.
func (c *Client) ListConfigMaps(ctx context.Context, namespace, selector string) ([]ConfigMap, error) {
	var out struct {
		Items []ConfigMap `json:"items"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps?labelSelector=" + url.QueryEscape(selector)
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetSecretData returns the decoded data of a secret.
func (c *Client) GetSecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := c.rest.Do(ctx, http.MethodGet, secretPath(namespace, name), nil, &s); err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(s.Data))
	for k, v := range s.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s key %s: %w", namespace, name, k, err)
		}
		out[k] = b
	}
	return out, nil
}

// AppProject is the part of an Argo CD AppProject that Squared reads.
type AppProject struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Roles []struct {
			Name     string   `json:"name"`
			Groups   []string `json:"groups"`
			Policies []string `json:"policies"`
		} `json:"roles"`
	} `json:"spec"`
	// Raw is the complete object, for checks of the field names.
	Raw json.RawMessage `json:"-"`
}

// ListAppProjects returns the Argo CD projects in a namespace.
func (c *Client) ListAppProjects(ctx context.Context, namespace string) ([]AppProject, error) {
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/appprojects"
	if err := c.rest.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	projects := make([]AppProject, 0, len(out.Items))
	for _, raw := range out.Items {
		var p AppProject
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("decode AppProject: %w", err)
		}
		p.Raw = raw
		projects = append(projects, p)
	}
	return projects, nil
}
