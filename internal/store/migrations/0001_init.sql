-- Initial schema of Squared.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    name          text NOT NULL,
    password_hash text NOT NULL,
    admin         boolean NOT NULL DEFAULT false,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- The database keeps only a SHA-256 hash of each session token. A copy of
-- the database thus does not give access to sessions.
CREATE TABLE sessions (
    token_hash   bytea PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,
    prefix       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz
);
CREATE INDEX api_tokens_user_id ON api_tokens (user_id);

CREATE TABLE tenants (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug           text NOT NULL UNIQUE,
    name           text NOT NULL,
    quota_cpu      text NOT NULL,
    quota_memory   text NOT NULL,
    quota_pods     integer NOT NULL,
    status         text NOT NULL,
    status_message text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'developer', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user_id ON memberships (user_id);

CREATE TABLE environments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name           text NOT NULL,
    namespace      text NOT NULL UNIQUE,
    status         text NOT NULL,
    status_message text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE deployments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments (id) ON DELETE RESTRICT,
    name           text NOT NULL,
    app_name       text NOT NULL UNIQUE,
    package_name   text NOT NULL,
    revision       integer NOT NULL DEFAULT 0,
    status         text NOT NULL,
    status_message text NOT NULL DEFAULT '',
    sync_status    text NOT NULL DEFAULT '',
    health_status  text NOT NULL DEFAULT '',
    synced_commit  text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (environment_id, name)
);
CREATE INDEX deployments_tenant_id ON deployments (tenant_id);

-- A revision is immutable after the worker commits it, except for the
-- status fields.
CREATE TABLE revisions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id   uuid NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    number          integer NOT NULL,
    package_name    text NOT NULL,
    package_version text NOT NULL,
    package         jsonb NOT NULL,
    "values"        jsonb NOT NULL,
    images          jsonb,
    commit_sha      text NOT NULL DEFAULT '',
    status          text NOT NULL,
    message         text NOT NULL DEFAULT '', -- Note from the user.
    error           text NOT NULL DEFAULT '', -- Last failure of the worker.
    created_by      uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, number)
);

-- The outbox makes the change of data and the queue message one atomic
-- operation. A relay sends the rows to RabbitMQ.
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

CREATE TABLE audit_events (
    id          bigserial PRIMARY KEY,
    tenant_id   uuid REFERENCES tenants (id) ON DELETE SET NULL,
    actor_id    uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_email text NOT NULL,
    action      text NOT NULL,
    target      text NOT NULL,
    detail      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_tenant_id ON audit_events (tenant_id, id DESC);
