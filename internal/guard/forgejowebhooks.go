package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// forgejoWebhook is one row of the view squared_guard_webhooks.
type forgejoWebhook struct {
	Scope     string // "repository", "owner", "system" or "default".
	Owner     string
	Repo      string
	Type      string // For example "gitea", "forgejo" or "slack".
	Active    bool
	URLSHA256 string // Hex SHA-256 of the address.
	URLHost   string
}

func (h forgejoWebhook) description() string {
	where := "a " + h.Scope + " webhook (all repositories)"
	switch h.Scope {
	case "repository":
		where = "a webhook of " + h.Owner + "/" + h.Repo
	case "owner":
		where = "a webhook of " + h.Owner
	}
	state := "active"
	if !h.Active {
		state = "inactive"
	}
	return fmt.Sprintf("%s (%s, %s) sends events to %s", where, h.Type, state, h.URLHost)
}

// urlSHA256 returns the hex SHA-256 of an address, as the view has it.
func urlSHA256(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// checkForgejoWebhooks checks the webhooks that see the repositories of
// the Squared organization: the webhooks of the organization and of its
// repositories, and the system and default webhooks, which apply to all
// repositories. A webhook sends commit messages, file names and authors to
// its address, and Forgejo makes the request from inside the cluster, thus
// a webhook can send GitOps data out, or send requests to inside services.
// allowed are the addresses that Squared expects, for example the webhook
// of Argo CD, or "sha256:<hex>" for an address with a secret. An inactive
// webhook counts too: somebody can turn it on.
func checkForgejoWebhooks(hooks []forgejoWebhook, org string, allowed []string) ([]string, error) {
	allowedHashes := map[string]string{}
	for _, u := range allowed {
		if hash, ok := strings.CutPrefix(u, "sha256:"); ok {
			allowedHashes[strings.ToLower(hash)] = u
		} else {
			allowedHashes[urlSHA256(u)] = u
		}
	}
	var warnings, problems []string
	found := map[string]bool{}
	for _, h := range hooks {
		if (h.Scope == "repository" || h.Scope == "owner") && !strings.EqualFold(h.Owner, org) {
			continue
		}
		if u, ok := allowedHashes[h.URLSHA256]; ok {
			if h.Active && h.Scope == "owner" {
				found[u] = true
			}
			continue
		}
		problems = append(problems, h.description())
	}
	for _, u := range slices.Sorted(slices.Values(allowed)) {
		if !found[u] {
			warnings = append(warnings, fmt.Sprintf("the organization %s has no active webhook to the allowed address with the host %s; Argo CD then sees new commits only when it polls",
				org, hostOf(u)))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("webhooks send the events of the GitOps repositories to addresses that Squared does not expect: %s. Remove the webhooks, or add their addresses to webhooks. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

// hostOf returns the host of an address, without a user or a password.
func hostOf(url string) string {
	if strings.HasPrefix(url, "sha256:") {
		return "(given as a hash)"
	}
	_, rest, ok := strings.Cut(url, "://")
	if !ok {
		return url
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest)
}
