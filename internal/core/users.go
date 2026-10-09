package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"squared/internal/auth"
	"squared/internal/domain"
	"squared/internal/store"
)

// Token prefixes. They make tokens easy to identify, for example by
// secret scanners.
const (
	sessionPrefix  = "sqs_"
	apiTokenPrefix = "sqp_"
)

// Authenticate checks an email address and a password. It returns
// domain.ErrUnauthenticated for all failures, so that the caller cannot
// see if the user exists.
func (s *Service) Authenticate(ctx context.Context, email, password string) (*domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(password) > 256 {
		return nil, domain.ErrUnauthenticated
	}
	u, err := s.store.UserByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		auth.CheckDummyPassword(password)
		return nil, domain.ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if !u.HasPassword() {
		// The user signs in only with the identity provider.
		auth.CheckDummyPassword(password)
		return nil, domain.ErrUnauthenticated
	}
	ok, err := auth.CheckPassword(u.PasswordHash, password)
	if err != nil {
		return nil, fmt.Errorf("check password of %s: %w", u.Email, err)
	}
	if !ok || u.Disabled {
		return nil, domain.ErrUnauthenticated
	}
	return u, nil
}

// NewSession makes a session for a user. idToken is the ID token of an
// OIDC sign-in, or "". It returns the session token for the cookie and
// the CSRF token for forms.
func (s *Service) NewSession(ctx context.Context, u *domain.User, idToken string) (token, csrf string, err error) {
	token = auth.NewToken(sessionPrefix)
	csrf = auth.NewToken("")
	expires := time.Now().Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(ctx, auth.HashToken(token), u.ID, csrf, idToken, expires); err != nil {
		return "", "", fmt.Errorf("create session: %w", err)
	}
	return token, csrf, nil
}

// SessionUser returns the user and session of a session token.
func (s *Service) SessionUser(ctx context.Context, token string) (*domain.User, *domain.Session, error) {
	if !strings.HasPrefix(token, sessionPrefix) {
		return nil, nil, domain.ErrUnauthenticated
	}
	sess, u, err := s.store.SessionByHash(ctx, auth.HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil, domain.ErrUnauthenticated
	}
	return u, sess, err
}

// EndSession removes a session.
func (s *Service) EndSession(ctx context.Context, token string) error {
	return s.store.DeleteSession(ctx, auth.HashToken(token))
}

// TokenUser returns the user of an API token.
func (s *Service) TokenUser(ctx context.Context, token string) (*domain.User, error) {
	if !strings.HasPrefix(token, apiTokenPrefix) {
		return nil, domain.ErrUnauthenticated
	}
	u, err := s.store.APITokenUser(ctx, auth.HashToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrUnauthenticated
	}
	return u, err
}

// CleanUp removes expired sessions and old outbox rows.
func (s *Service) CleanUp(ctx context.Context) error {
	if _, err := s.store.DeleteExpiredSessions(ctx); err != nil {
		return err
	}
	if err := s.store.DeleteExpiredOIDCLogins(ctx); err != nil {
		return err
	}
	_, err := s.store.PurgeOutbox(ctx, 7*24*time.Hour)
	return err
}

// CreateAPIToken makes an API token for the actor. The token is only
// returned here. The database keeps only its hash.
func (s *Service) CreateAPIToken(ctx context.Context, actor *domain.User, name string, days int) (string, *domain.APIToken, error) {
	if actor == nil {
		return "", nil, domain.ErrUnauthenticated
	}
	var verr domain.ValidationError
	if err := domain.ValidateDisplayName(name); err != nil {
		verr.Add("name", err.Error())
	}
	if days < 0 || days > 366 {
		verr.Add("days", "must be from 1 to 366, or 0 for no expiry")
	}
	if err := verr.Err(); err != nil {
		return "", nil, err
	}
	token := auth.NewToken(apiTokenPrefix)
	t := &domain.APIToken{UserID: actor.ID, Name: name, Prefix: token[:len(apiTokenPrefix)+6]}
	if days > 0 {
		exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		t.ExpiresAt = &exp
	}
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.CreateAPIToken(ctx, t, auth.HashToken(token)); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "", "token.create", actor.Email, name)
	})
	if err != nil {
		return "", nil, err
	}
	return token, t, nil
}

// ListAPITokens returns the API tokens of the actor.
func (s *Service) ListAPITokens(ctx context.Context, actor *domain.User) ([]domain.APIToken, error) {
	if actor == nil {
		return nil, domain.ErrUnauthenticated
	}
	return s.store.ListAPITokens(ctx, actor.ID)
}

// DeleteAPIToken removes an API token of the actor.
func (s *Service) DeleteAPIToken(ctx context.Context, actor *domain.User, id string) error {
	if actor == nil {
		return domain.ErrUnauthenticated
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.DeleteAPIToken(ctx, actor.ID, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "", "token.delete", actor.Email, id)
	})
}

// ChangePassword sets a new password for the actor. It ends all sessions
// of the actor.
func (s *Service) ChangePassword(ctx context.Context, actor *domain.User, current, next string) error {
	if actor == nil {
		return domain.ErrUnauthenticated
	}
	if !actor.HasPassword() {
		return domain.Invalid("current", "your account has no password; the identity provider manages it")
	}
	if _, err := s.Authenticate(ctx, actor.Email, current); err != nil {
		return domain.Invalid("current", "is not correct")
	}
	if err := domain.ValidatePassword(next); err != nil {
		return domain.Invalid("password", err.Error())
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.UpdatePassword(ctx, actor.ID, hash); err != nil {
			return err
		}
		if err := tx.DeleteUserSessions(ctx, actor.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "", "user.password", actor.Email, "")
	})
}

// NewUser is the input to CreateUser.
type NewUser struct {
	Email    string
	Name     string
	Password string
	Admin    bool
}

func (in *NewUser) validate(passwordless bool) error {
	var verr domain.ValidationError
	email, err := domain.NormalizeEmail(in.Email)
	if err != nil {
		verr.Add("email", err.Error())
	}
	in.Email = email
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateDisplayName(in.Name); err != nil {
		verr.Add("name", err.Error())
	}
	if in.Password != "" || !passwordless {
		if err := domain.ValidatePassword(in.Password); err != nil {
			verr.Add("password", err.Error())
		}
	}
	return verr.Err()
}

// CreateUser adds a user. Only platform administrators can add users.
// With SSO, the password is optional: a user without a password is linked
// to the identity with the same verified email address at the first
// sign-in.
func (s *Service) CreateUser(ctx context.Context, actor *domain.User, in NewUser) (*domain.User, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	if err := in.validate(s.cfg.SSO); err != nil {
		return nil, err
	}
	hash := ""
	if in.Password != "" {
		var err error
		if hash, err = auth.HashPassword(in.Password); err != nil {
			return nil, err
		}
	}
	u := &domain.User{Email: in.Email, Name: in.Name, PasswordHash: hash, Admin: in.Admin}
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.CreateUser(ctx, u); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return domain.Invalid("email", "a user with this address exists")
			}
			return err
		}
		detail := "user"
		if u.Admin {
			detail = "administrator"
		}
		return s.audit(ctx, tx, actor, "", "user.create", u.Email, detail)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

// BootstrapAdmin adds the first platform administrator if there are no
// users. It does nothing if a user exists.
func (s *Service) BootstrapAdmin(ctx context.Context, email, password string) (bool, error) {
	in := NewUser{Email: email, Name: "Administrator", Password: password, Admin: true}
	if err := in.validate(false); err != nil {
		return false, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return false, err
	}
	created := false
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		n, err := tx.CountUsers(ctx)
		if err != nil || n > 0 {
			return err
		}
		u := &domain.User{Email: in.Email, Name: in.Name, PasswordHash: hash, Admin: true}
		if err := tx.CreateUser(ctx, u); err != nil {
			return err
		}
		created = true
		return s.audit(ctx, tx, nil, "", "user.create", u.Email, "first administrator")
	})
	return created, err
}

// ListUsers returns all users. Only platform administrators can list users.
func (s *Service) ListUsers(ctx context.Context, actor *domain.User) ([]domain.User, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return s.store.ListUsers(ctx)
}

// SetUserDisabled enables or disables a user. A disabled user cannot sign
// in, and all sessions of the user end.
func (s *Service) SetUserDisabled(ctx context.Context, actor *domain.User, userID string, disabled bool) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if userID == actor.ID {
		return domain.Invalid("user", "you cannot disable your own account")
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		u, err := tx.UserByID(ctx, userID)
		if err != nil {
			return err
		}
		if err := tx.SetUserDisabled(ctx, userID, disabled); err != nil {
			return err
		}
		action := "user.enable"
		if disabled {
			action = "user.disable"
			if err := tx.DeleteUserSessions(ctx, userID); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, actor, "", action, u.Email, "")
	})
}

// ResetPassword sets a new password for another user. Only platform
// administrators can do this.
func (s *Service) ResetPassword(ctx context.Context, actor *domain.User, userID, password string) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	if err := domain.ValidatePassword(password); err != nil {
		return domain.Invalid("password", err.Error())
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx *store.Store) error {
		u, err := tx.UserByID(ctx, userID)
		if err != nil {
			return err
		}
		if err := tx.UpdatePassword(ctx, userID, hash); err != nil {
			return err
		}
		if err := tx.DeleteUserSessions(ctx, userID); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "", "user.password", u.Email, "reset by administrator")
	})
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
