package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"squared/internal/auth"
	"squared/internal/domain"
	"squared/internal/store"
)

// Identity is a verified identity from the OpenID Connect provider.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	// Admin is the administrator flag from the provider. It is nil if the
	// provider does not control the flag.
	Admin *bool
}

// ErrNoVerifiedEmail tells that the provider did not give a verified email
// address for a new user.
var ErrNoVerifiedEmail = errors.New("the identity provider gave no verified email address")

// SignInIdentity returns the user of an identity. The steps are:
//
//  1. A user that is linked to the identity is the result.
//  2. Else, a user with the same verified email address and no linked
//     identity is linked. An administrator can thus add a user before the
//     first sign-in.
//  3. Else, Squared adds a new user without a password.
//
// If the provider controls the administrator flag, the flag of the user
// changes to the value from the provider. via tells where the identity
// came from, for the audit log: "web" or "api".
func (s *Service) SignInIdentity(ctx context.Context, id Identity, via string) (*domain.User, error) {
	if id.Issuer == "" || id.Subject == "" {
		return nil, errors.New("identity without issuer or subject")
	}
	email := ""
	if id.EmailVerified {
		if e, err := domain.NormalizeEmail(id.Email); err == nil {
			email = e
		}
	}
	name := strings.TrimSpace(id.Name)
	if domain.ValidateDisplayName(name) != nil {
		name = email
	}

	var result *domain.User
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		u, err := tx.UserByIdentity(ctx, id.Issuer, id.Subject)
		switch {
		case err == nil:
			// Keep the profile up to date. A change of the email address
			// fails if another user has the address; then keep the old one.
			if email != "" && (email != u.Email || (name != "" && name != u.Name)) {
				newName := u.Name
				if name != "" {
					newName = name
				}
				if err := tx.UpdateProfile(ctx, u.ID, email, newName); err == nil {
					u.Email, u.Name = email, newName
				} else if !errors.Is(err, domain.ErrConflict) {
					return err
				}
			}
		case errors.Is(err, domain.ErrNotFound):
			if email == "" {
				return ErrNoVerifiedEmail
			}
			u, err = tx.UserByEmail(ctx, email)
			switch {
			case err == nil && u.SSO():
				return fmt.Errorf("%w: the address %s belongs to a different identity", domain.ErrConflict, email)
			case err == nil:
				if err := tx.LinkIdentity(ctx, u.ID, id.Issuer, id.Subject); err != nil {
					return err
				}
				u.OIDCIssuer, u.OIDCSubject = id.Issuer, id.Subject
				if err := s.audit(ctx, tx, u, "", "user.link", u.Email, "identity "+id.Subject+" ("+via+")"); err != nil {
					return err
				}
			case errors.Is(err, domain.ErrNotFound):
				if name == "" {
					name = email
				}
				u = &domain.User{Email: email, Name: name, OIDCIssuer: id.Issuer, OIDCSubject: id.Subject}
				if id.Admin != nil {
					u.Admin = *id.Admin
				}
				if err := tx.CreateUser(ctx, u); err != nil {
					return err
				}
				if err := s.audit(ctx, tx, u, "", "user.create", u.Email, "first sign-in with the identity provider ("+via+")"); err != nil {
					return err
				}
			default:
				return err
			}
		default:
			return err
		}
		if u.Disabled {
			return domain.ErrUnauthenticated
		}
		if id.Admin != nil && *id.Admin != u.Admin {
			if err := tx.SetUserAdmin(ctx, u.ID, *id.Admin); err != nil {
				return err
			}
			u.Admin = *id.Admin
			detail := "administrator rights removed by the identity provider"
			if u.Admin {
				detail = "administrator rights given by the identity provider"
			}
			if err := s.audit(ctx, tx, u, "", "user.admin", u.Email, detail); err != nil {
				return err
			}
		}
		result = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// oidcLoginTTL is the time that a user has to sign in at the provider.
const oidcLoginTTL = 10 * time.Minute

// StartOIDCLogin saves a sign-in attempt and returns the token for the
// browser cookie.
func (s *Service) StartOIDCLogin(ctx context.Context, l store.OIDCLogin) (string, error) {
	token := auth.NewToken("sql_")
	if err := s.store.CreateOIDCLogin(ctx, auth.HashToken(token), l, time.Now().Add(oidcLoginTTL)); err != nil {
		return "", err
	}
	return token, nil
}

// FinishOIDCLogin returns the sign-in attempt of a browser cookie token
// and removes it. It returns domain.ErrNotFound if the attempt does not
// exist or expired.
func (s *Service) FinishOIDCLogin(ctx context.Context, token string) (*store.OIDCLogin, error) {
	return s.store.TakeOIDCLogin(ctx, auth.HashToken(token))
}
