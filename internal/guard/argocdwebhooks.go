package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
)

// webhookProbe is a push event of a Git server, without a signature, for
// a repository that does not exist. Argo CD then reads no repository and
// changes nothing.
type webhookProbe struct {
	Provider string // The name for the messages.
	Secret   string // The key of the secret in argocd-secret.
	Header   string // The header that tells the kind of event.
	Value    string
	Body     string
}

const probeRepo = `"repository": {"html_url": "https://git.example.invalid/squared-guard/probe", ` +
	`"clone_url": "https://git.example.invalid/squared-guard/probe.git", "full_name": "squared-guard/probe", ` +
	`"git_http_url": "https://git.example.invalid/squared-guard/probe.git", "web_url": "https://git.example.invalid/squared-guard/probe", ` +
	`"homepage": "https://git.example.invalid/squared-guard/probe", "url": "https://git.example.invalid/squared-guard/probe"}`

var (
	probeGitHub = webhookProbe{"GitHub", "webhook.github.secret", "X-GitHub-Event", "push",
		`{"ref": "refs/heads/main", ` + probeRepo + `}`}
	probeGitLab = webhookProbe{"GitLab", "webhook.gitlab.secret", "X-Gitlab-Event", "Push Hook",
		`{"ref": "refs/heads/main", ` + probeRepo + `}`}
	probeGogs = webhookProbe{"Gogs, Gitea and Forgejo", "webhook.gogs.secret", "X-Gogs-Event", "push",
		`{"ref": "refs/heads/main", ` + probeRepo + `}`}
	probeAzure = webhookProbe{"Azure DevOps", "webhook.azuredevops.username and webhook.azuredevops.password", "X-Vss-ActivityId", "squared-guard",
		`{"eventType": "git.push", "resource": {"refUpdates": [{"name": "refs/heads/main"}], ` +
			`"repository": {"remoteUrl": "https://dev.azure.example.invalid/squared-guard/_git/probe", "defaultBranch": "refs/heads/main"}}}`}
	probeBitbucketServer = webhookProbe{"Bitbucket Server", "webhook.bitbucketserver.secret", "X-Event-Key", "repo:refs_changed",
		`{"eventKey": "repo:refs_changed", "repository": {"slug": "probe", "project": {"key": "GUARD"}, ` +
			`"links": {"clone": [{"href": "https://bitbucket.example.invalid/scm/guard/probe.git", "name": "http"}]}}, ` +
			`"changes": [{"ref": {"id": "refs/heads/main", "displayId": "main", "type": "BRANCH"}}]}`}
)

// appSetWebhookProbes are the providers of the webhook of the ApplicationSet
// controller, and serverWebhookProbes those of the API server.
var (
	appSetWebhookProbes = []webhookProbe{probeGitHub, probeGitLab, probeAzure}
	serverWebhookProbes = []webhookProbe{probeGitHub, probeGitLab, probeGogs, probeAzure, probeBitbucketServer}
)

// errNotListening tells that nothing listens at the address of a webhook.
var errNotListening = errors.New("nothing listens")

// probeWebhook sends the probes to a webhook (url), and returns the probes
// that it accepts. Argo CD accepts an event without a signature only when
// the secret of its provider is empty.
func probeWebhook(ctx context.Context, client *http.Client, url string, probes []webhookProbe) ([]webhookProbe, error) {
	var accepted []webhookProbe
	for _, p := range probes {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(p.Body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(p.Header, p.Value)
		resp, err := client.Do(req)
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errNotListening
		}
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			accepted = append(accepted, p)
		}
	}
	return accepted, nil
}

// webhookWarnings returns the warnings for the probes that a webhook (who)
// accepts. The webhook then accepts each event of the provider without a
// signature. Such an event cannot change applications, but each pod that
// can connect can make Argo CD read the repositories again, and the
// ApplicationSet controller run the generators again, as often as it
// wants. restart tells that the component reads the secrets only at its
// start.
func webhookWarnings(who string, accepted []webhookProbe, restart bool) []string {
	var w []string
	for _, p := range accepted {
		m := fmt.Sprintf("the webhook of %s accepts events of %s without a signature: %s in argocd-secret is empty. Each pod that can connect can make Argo CD read the repositories again",
			who, p.Provider, p.Secret)
		if restart {
			m += ". The controller reads the secrets only at its start, thus restart it after a change"
		}
		w = append(w, m)
	}
	return w
}
