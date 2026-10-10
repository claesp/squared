package guard

import (
	"fmt"
	"slices"
	"strings"

	"squared/internal/keycloak"
)

// checkRealmEmail checks that a person can get only an email address that
// the person owns, or that an administrator gave. Squared links a new
// sign-in to an existing account with the same verified address, and the
// worker finds the Keycloak user of a tenant member by the address. Thus
// an address that a person can choose gives the account or the access of
// another person. ignore are identity providers and user federation
// providers that an operator checked.
func checkRealmEmail(r keycloak.RealmEmail, ignore []string) ([]string, error) {
	var warnings, problems []string
	if r.DuplicateEmailsAllowed {
		problems = append(problems, "duplicateEmailsAllowed is on: a person can have the address of another person")
	}
	if r.RegistrationAllowed && !r.VerifyEmail {
		problems = append(problems, "registrationAllowed is on without verifyEmail: anybody can make an account with any address that Keycloak does not have")
	}
	if r.RegistrationEmailAsUsername && r.EditUsernameAllowed {
		problems = append(problems, "registrationEmailAsUsername and editUsernameAllowed are on: users can change the user name, which is the address")
	}
	if r.EmailEditableByUser {
		problems = append(problems, `users can change their address (the attribute email of the user profile has "user" in permissions.edit)`)
	}
	for _, idp := range r.IdentityProviders {
		if slices.Contains(ignore, idp.Alias) {
			continue
		}
		if idp.TrustEmail {
			problems = append(problems, fmt.Sprintf("identity provider %s has trustEmail on: its addresses count as verified", idp.Alias))
		} else {
			warnings = append(warnings, fmt.Sprintf("Keycloak identity provider %s gives the addresses of its users; check that it verifies them, then add %s to ignoreFields", idp.Alias, idp.Alias))
		}
	}
	for _, fed := range r.UserFederation {
		if slices.Contains(ignore, fed.Name) {
			continue
		}
		if fed.TrustEmail {
			problems = append(problems, fmt.Sprintf("user federation %s has trustEmail on: its addresses count as verified", fed.Name))
		} else {
			warnings = append(warnings, fmt.Sprintf("Keycloak user federation %s (%s) gives the addresses of its users; check that users cannot change them, then add %s to ignoreFields", fed.Name, fed.ProviderID, fed.Name))
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the email settings of the Keycloak realm let a person choose an address: %s. Squared links accounts by the address, and the worker gives tenant access by it. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
