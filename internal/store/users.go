package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
)

const userColumns = "id, email, name, password_hash, admin, disabled, created_at"

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Admin, &u.Disabled, &u.CreatedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &u, nil
}

// CreateUser adds a user. It sets u.ID and u.CreatedAt.
func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	err := s.db.QueryRow(ctx, `INSERT INTO users (email, name, password_hash, admin)
		VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		u.Email, u.Name, u.PasswordHash, u.Admin).Scan(&u.ID, &u.CreatedAt)
	return mapErr(err)
}

// UserByEmail returns the user with an email address.
func (s *Store) UserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return scanUser(s.db.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE email = $1", email))
}

// UserByID returns the user with an ID.
func (s *Store) UserByID(ctx context.Context, id string) (*domain.User, error) {
	return scanUser(s.db.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE id = $1", id))
}

// ListUsers returns all users, ordered by email.
func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.Query(ctx, "SELECT "+userColumns+" FROM users ORDER BY email")
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *u)
	}
	return users, mapErr(rows.Err())
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, mapErr(err)
}

// UpdatePassword sets the password hash of a user.
func (s *Store) UpdatePassword(ctx context.Context, userID, hash string) error {
	return s.execOne(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", userID, hash)
}

// SetUserDisabled enables or disables a user.
func (s *Store) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	return s.execOne(ctx, "UPDATE users SET disabled = $2 WHERE id = $1", userID, disabled)
}

// SetUserAdmin gives or removes platform administrator rights.
func (s *Store) SetUserAdmin(ctx context.Context, userID string, admin bool) error {
	return s.execOne(ctx, "UPDATE users SET admin = $2 WHERE id = $1", userID, admin)
}

// CountActiveAdmins returns the number of enabled platform administrators.
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM users WHERE admin AND NOT disabled").Scan(&n)
	return n, mapErr(err)
}

// execOne runs a statement that must change exactly one row.
func (s *Store) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := s.db.Exec(ctx, sql, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

// CreateSession adds a session.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID, csrf string, expires time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at)
		VALUES ($1, $2, $3, $4)`, tokenHash, userID, csrf, expires)
	return mapErr(err)
}

// SessionByHash returns a session that did not expire and its user. It
// returns domain.ErrNotFound if the user is disabled.
func (s *Store) SessionByHash(ctx context.Context, tokenHash []byte) (*domain.Session, *domain.User, error) {
	var sess domain.Session
	var u domain.User
	err := s.db.QueryRow(ctx, `SELECT s.user_id, s.csrf_token, s.expires_at,
			u.id, u.email, u.name, u.password_hash, u.admin, u.disabled, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND NOT u.disabled`, tokenHash).Scan(
		&sess.UserID, &sess.CSRFToken, &sess.ExpiresAt,
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.Admin, &u.Disabled, &u.CreatedAt)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	return &sess, &u, nil
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", tokenHash)
	return mapErr(err)
}

// DeleteUserSessions removes all sessions of a user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", userID)
	return mapErr(err)
}

// DeleteExpiredSessions removes sessions that expired.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= now()")
	return tag.RowsAffected(), mapErr(err)
}

// CreateAPIToken adds an API token. It sets t.ID and t.CreatedAt.
func (s *Store) CreateAPIToken(ctx context.Context, t *domain.APIToken, tokenHash []byte) error {
	err := s.db.QueryRow(ctx, `INSERT INTO api_tokens (user_id, name, token_hash, prefix, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
		t.UserID, t.Name, tokenHash, t.Prefix, t.ExpiresAt).Scan(&t.ID, &t.CreatedAt)
	return mapErr(err)
}

// APITokenUser returns the user of a valid API token and records the use.
// It returns domain.ErrNotFound if the token expired or the user is
// disabled.
func (s *Store) APITokenUser(ctx context.Context, tokenHash []byte) (*domain.User, error) {
	return scanUser(s.db.QueryRow(ctx, `UPDATE api_tokens t SET last_used_at = now()
		FROM users u
		WHERE t.token_hash = $1 AND u.id = t.user_id AND NOT u.disabled
		  AND (t.expires_at IS NULL OR t.expires_at > now())
		RETURNING u.id, u.email, u.name, u.password_hash, u.admin, u.disabled, u.created_at`, tokenHash))
}

// ListAPITokens returns the API tokens of a user.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]domain.APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT id, user_id, name, prefix, created_at, expires_at, last_used_at
		FROM api_tokens WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var tokens []domain.APIToken
	for rows.Next() {
		var t domain.APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, mapErr(err)
		}
		tokens = append(tokens, t)
	}
	return tokens, mapErr(rows.Err())
}

// DeleteAPIToken removes an API token of a user.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, tokenID string) error {
	return s.execOne(ctx, "DELETE FROM api_tokens WHERE id = $1 AND user_id = $2", tokenID, userID)
}
