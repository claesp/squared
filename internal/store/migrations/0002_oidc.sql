-- Sign-in with an OpenID Connect provider.

-- A user is linked to one identity of the provider: the issuer and the
-- subject. A user without a password ('') can only sign in with the
-- provider.
ALTER TABLE users ADD COLUMN oidc_issuer text;
ALTER TABLE users ADD COLUMN oidc_subject text;
ALTER TABLE users ADD CONSTRAINT users_oidc_identity UNIQUE (oidc_issuer, oidc_subject);

-- The ID token of the session. Sign-out sends it to the provider.
ALTER TABLE sessions ADD COLUMN id_token text NOT NULL DEFAULT '';

-- A sign-in attempt that waits for the provider. The browser has the
-- token in a cookie; the database keeps only its hash. A row is used one
-- time.
CREATE TABLE oidc_logins (
    token_hash bytea PRIMARY KEY,
    state      text NOT NULL,
    nonce      text NOT NULL,
    verifier   text NOT NULL,
    next       text NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX oidc_logins_expires_at ON oidc_logins (expires_at);
