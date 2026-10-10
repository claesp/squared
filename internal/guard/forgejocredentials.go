package guard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"squared/internal/keycloak"
)

// forgejoCredentialOwner is one row of the view
// squared_guard_credential_owners: a user of an OAuth2 authentication
// source with personal access tokens or SSH keys.
type forgejoCredentialOwner struct {
	UserName        string
	LoginSource     int64
	LoginName       string // The user ID at the identity provider (sub).
	IsAdmin         bool
	IsActive        bool
	ProhibitLogin   bool
	Tokens          int
	TokensLastUsed  time.Time
	SSHKeys         int
	SSHKeysLastUsed time.Time
}

// credentials describes the credentials of an owner for messages.
func (o forgejoCredentialOwner) credentials() string {
	var parts []string
	if o.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("personal access tokens (%d, last use %s)", o.Tokens, o.TokensLastUsed.UTC().Format(time.DateOnly)))
	}
	if o.SSHKeys > 0 {
		parts = append(parts, fmt.Sprintf("SSH keys (%d, last use %s)", o.SSHKeys, o.SSHKeysLastUsed.UTC().Format(time.DateOnly)))
	}
	return strings.Join(parts, " and ")
}

// keycloakUserState is what the guard reads of a Keycloak user.
type keycloakUserState struct {
	Exists  bool
	Enabled bool
	Groups  []string // Full paths of the direct memberships, as in the groups claim.
}

// checkForgejoCredentials checks the owners of personal access tokens and
// SSH keys. These credentials work without Keycloak: the API and Git
// accept a token, and Git over SSH accepts a key, until somebody deletes
// it. A key also stays when SSH is off, and works when somebody turns SSH
// on. Thus a credential must not outlive the Keycloak user, and must not
// keep administrator rights that Keycloak took away: Forgejo changes the
// administrator flag only at a Keycloak sign-in. sourceID is the Keycloak
// authentication source in Forgejo, and adminGroup its administrator group
// (empty: Forgejo does not take the flag from Keycloak). Forgejo refuses
// all requests of an account that is not active or cannot sign in, thus
// the credentials of such an account are no problem.
func checkForgejoCredentials(ctx context.Context, owners []forgejoCredentialOwner, sourceID int64, adminGroup string,
	user func(ctx context.Context, id string) (keycloakUserState, error)) ([]string, error) {
	var warnings, problems []string
	other := map[int64]bool{}
	for _, o := range owners {
		if !o.IsActive || o.ProhibitLogin {
			continue
		}
		if o.LoginSource != sourceID {
			if !other[o.LoginSource] {
				other[o.LoginSource] = true
				warnings = append(warnings, fmt.Sprintf("users of the Forgejo authentication source %d have personal access tokens or SSH keys; the guard checks only the users of source %d", o.LoginSource, sourceID))
			}
			continue
		}
		state, err := user(ctx, o.LoginName)
		if err != nil {
			return nil, err
		}
		creds := o.credentials()
		switch {
		case !state.Exists:
			problems = append(problems, fmt.Sprintf("Forgejo user %s has %s, but Keycloak has no user %s", o.UserName, creds, o.LoginName))
		case !state.Enabled:
			problems = append(problems, fmt.Sprintf("Forgejo user %s has %s, but Keycloak disabled the user", o.UserName, creds))
		case o.IsAdmin && adminGroup != "" && !slices.Contains(state.Groups, adminGroup):
			problems = append(problems, fmt.Sprintf("Forgejo user %s is a Forgejo administrator, but not a member of %s in Keycloak; its %s give the administrator rights", o.UserName, adminGroup, creds))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("credentials in Forgejo give rights that Keycloak does not give: %s. Delete the tokens and keys, or block the Forgejo account (prohibit sign-in). The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

// keycloakUserStateFrom reads a Keycloak user for checkForgejoCredentials.
func keycloakUserStateFrom(idp KeycloakReader) func(ctx context.Context, id string) (keycloakUserState, error) {
	return func(ctx context.Context, id string) (keycloakUserState, error) {
		u, err := idp.UserByID(ctx, id)
		if errors.Is(err, keycloak.ErrUserNotFound) {
			return keycloakUserState{}, nil
		}
		if err != nil {
			return keycloakUserState{}, err
		}
		groups, err := idp.UserGroupPaths(ctx, id)
		if err != nil {
			return keycloakUserState{}, err
		}
		return keycloakUserState{Exists: true, Enabled: u.Enabled, Groups: groups}, nil
	}
}
