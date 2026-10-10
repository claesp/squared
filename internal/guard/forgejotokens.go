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

// forgejoTokenOwner is one row of the view squared_guard_token_owners: a
// user of an OAuth2 authentication source with personal access tokens.
type forgejoTokenOwner struct {
	UserName      string
	LoginSource   int64
	LoginName     string // The user ID at the identity provider (sub).
	IsAdmin       bool
	IsActive      bool
	ProhibitLogin bool
	Tokens        int
	LastUsed      time.Time
}

// keycloakUserState is what the guard reads of a Keycloak user.
type keycloakUserState struct {
	Exists  bool
	Enabled bool
	Groups  []string // Full paths of the direct memberships, as in the groups claim.
}

// checkForgejoTokens checks the owners of personal access tokens. A token
// works without Keycloak: the API and Git accept it until somebody deletes
// it. Thus a token must not outlive the Keycloak user, and must not keep
// administrator rights that Keycloak took away: Forgejo changes the
// administrator flag only at a Keycloak sign-in. sourceID is the Keycloak
// authentication source in Forgejo, and adminGroup its administrator group
// (empty: Forgejo does not take the flag from Keycloak). Forgejo refuses
// all requests of an account that is not active or cannot sign in, thus
// the tokens of such an account are no problem.
func checkForgejoTokens(ctx context.Context, owners []forgejoTokenOwner, sourceID int64, adminGroup string,
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
				warnings = append(warnings, fmt.Sprintf("users of the Forgejo authentication source %d have personal access tokens; the guard checks only the users of source %d", o.LoginSource, sourceID))
			}
			continue
		}
		state, err := user(ctx, o.LoginName)
		if err != nil {
			return nil, err
		}
		tokens := fmt.Sprintf("personal access tokens (%d, last use %s)", o.Tokens, o.LastUsed.UTC().Format(time.DateOnly))
		switch {
		case !state.Exists:
			problems = append(problems, fmt.Sprintf("Forgejo user %s has %s, but Keycloak has no user %s", o.UserName, tokens, o.LoginName))
		case !state.Enabled:
			problems = append(problems, fmt.Sprintf("Forgejo user %s has %s, but Keycloak disabled the user", o.UserName, tokens))
		case o.IsAdmin && adminGroup != "" && !slices.Contains(state.Groups, adminGroup):
			problems = append(problems, fmt.Sprintf("Forgejo user %s is a Forgejo administrator, but not a member of %s in Keycloak; its %s give the administrator rights", o.UserName, adminGroup, tokens))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("personal access tokens of Forgejo give rights that Keycloak does not give: %s. Delete the tokens, or block the Forgejo account (prohibit sign-in). The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

// keycloakUserStateFrom reads a Keycloak user for checkForgejoTokens.
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
