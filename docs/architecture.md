# Architecture

This document describes the parts of Squared and how they work together.
It also records the important design decisions and the reasons for them.

## Glossary

| Term | Meaning |
|---|---|
| Tenant | One customer of the platform. A tenant has members, environments, a Git repository and a registry project. |
| Environment | One Kubernetes namespace of a tenant, for example `dev` or `prod`. The namespace name is `<tenant>-<environment>`. |
| Package | A versioned definition from the catalog. A package has parameters and one or more components. |
| Component | One part of a package, for example a web server. The component type selects the renderer: `web`, `worker` or `cron`. |
| Deployment | One instance of a package in an environment. |
| Revision | One immutable set of inputs for a deployment: a copy of the package, the parameter values and the image digests. |
| Task | A message from the API server to the worker, for example "synchronize deployment X". |
| Identity provider | The OpenID Connect server that signs users in, for example Keycloak. |
| Platform repository | The Git repository `squared/platform`. It has the namespaces, limits and Argo CD objects of all tenants. |
| Tenant repository | The Git repository `squared/tenant-<tenant>`. It has the Kubernetes manifests of the deployments of one tenant. |

## Parts

```
                                     +----------+
              sign-in (redirect) ...>| Keycloak |<... token, keys (server)
                     .               +----------+
             Browser (HTML, no JavaScript)       API clients (bearer token)
                          \                          /
                           v                        v
                   +------------------------------------+
                   |  squared serve                     |
                   |  web interface, JSON API,          |      +------------+
                   |  outbox relay                      |----->| PostgreSQL |
                   +------------------------------------+      +------------+
                                    |  tasks (publisher confirms)     ^
                                    v                                 |
                             +-------------+                          |
                             |  RabbitMQ   |                          |
                             +-------------+                          |
                                    |  tasks (manual acknowledgement) |
                                    v                                 |
                   +------------------------------------+             |
                   |  squared worker                    |-------------+
                   |  task handlers, status watcher     |
                   +------------------------------------+
                      |              |               |
              projects, robots   commits      pull secrets (write)
              digests            (REST API)   applications (read)
                      v              v               v
                 +--------+     +---------+     +----------------+
                 | Harbor |     | Forgejo |     | Kubernetes API |
                 +--------+     +---------+     +----------------+
                      ^              |  webhook          ^
                      | pull         v                   | apply
                 +------------+  +---------+             |
                 | kubelet    |  | Argo CD |-------------+
                 +------------+  +---------+
```

Squared is one Go binary with two roles:

- **`squared serve`** has the web interface, the JSON API and the outbox
  relay. It reads and writes PostgreSQL and sends tasks to RabbitMQ. It has
  no credentials for Forgejo, Harbor or Kubernetes.
- **`squared worker`** gets tasks from RabbitMQ and changes the external
  systems. It also copies the status of the Argo CD applications into
  PostgreSQL. The web interface reads the status from PostgreSQL.

Both roles use the same `core.Service` for business rules. Thus the web
interface and the API have the same validation and the same permission
checks.

## Flow of a sign-in

Squared uses the OpenID Connect authorization code flow with PKCE:

1. The user selects **Sign in with Keycloak**. The server saves a sign-in
   attempt with a random state, nonce and PKCE code verifier. The browser
   gets a cookie that identifies the attempt.
2. The server sends the browser to the authorization endpoint of Keycloak.
3. The user signs in at Keycloak. Squared never sees the password.
4. Keycloak sends the browser back to `/auth/callback` with a code and the
   state. The server takes the attempt from the database (one time only)
   and compares the state.
5. The server changes the code into tokens at the token endpoint. It sends
   the client secret and the PKCE code verifier.
6. The server verifies the ID token: signature, issuer, audience, expiry
   time and nonce.
7. The server finds or makes the user (see below) and starts a session.

To find the user, Squared looks for the user that is linked to the issuer
and subject of the identity. If there is no such user, Squared links the
user with the same verified email address, if that user has no other
identity. Else Squared makes a new user without a password. If an
administrator group is configured, Squared sets the administrator flag
from the groups claim at each sign-in.

Sign-out ends the session in Squared and sends the browser to the end
session endpoint of Keycloak with the ID token. Thus the Keycloak session
also ends.

## Flow of a deployment

1. A user sends the deployment form, or calls `POST /api/v1/tenants/{tenant}/deployments`.
2. The server checks the permission and the parameter values. It writes the
   deployment, revision 1, an audit event and a task into PostgreSQL in one
   transaction.
3. The outbox relay sends the task to RabbitMQ and marks the outbox row.
4. The worker gets the task. It reads the desired revision from PostgreSQL.
5. The worker asks Harbor for the digest of each image tag. It saves the
   digests in the revision. The digests do not change after this step.
6. The worker renders the manifests and commits them to
   `<environment>/<deployment>/` in the tenant repository.
7. The worker commits an Argo CD application to the platform repository.
   The application follows the commit of step 6, not a branch.
8. Forgejo sends a webhook to Argo CD. The root application applies the new
   application. The new application applies the manifests.
9. The status watcher of the worker reads the application status every
   few seconds. When Argo CD reports "Synced" and "Healthy" for the correct
   commit, the deployment status changes to `deployed`.

## Status values

| Status | Meaning |
|---|---|
| `pending` | Squared saved the change. The worker did not process it yet. |
| `provisioning` | The worker prepares the registry, the repository and the namespaces of a tenant. |
| `ready` | The tenant or the environment is ready for use. |
| `committed` | The manifests are in Git. Argo CD did not apply them completely yet. |
| `deployed` | Argo CD applied the manifests and all objects are healthy. |
| `degraded` | Argo CD applied the manifests, but an object is not healthy, or the sync failed. |
| `failed` | The worker stopped after the maximum number of attempts, or the input is not valid. |
| `deleting` | The worker removes the deployment or the tenant. |

## Decisions

### D1: Argo CD applies all workloads; Squared does not write workloads to the Kubernetes API

Squared could apply manifests to the Kubernetes API directly. That needs a
service account that can write most object kinds in all tenant namespaces.
If an attacker gets that token, the attacker controls all tenants.

Instead, Squared writes the desired state to Git. Argo CD applies it. This
gives:

- **Security.** The worker has no write access to workloads. Argo CD
  projects limit each tenant to its own namespaces and to a fixed list of
  object kinds. Tenants cannot make cluster objects, roles or secrets.
- **Reproducibility.** Git has the exact manifests of each revision.
- **Audit.** Each change is a commit with the revision number and the user.
- **Drift repair.** Argo CD changes back manual changes in the cluster
  (`selfHeal`).

### D2: Squared writes only image pull secrets to the Kubernetes API

Secrets must not be in Git. The image pull secret of a tenant is thus the
only object that the worker writes to the Kubernetes API. The worker can
write secrets only in tenant namespaces: each tenant namespace has a Role
and a RoleBinding from the platform repository. In the namespace of Argo
CD, the worker can only read applications.

### D3: An application follows a commit, not a branch

Each Argo CD application has `targetRevision: <commit>`. A deployment thus
changes only when Squared commits a new application file. A commit of one
deployment cannot change another deployment. The status watcher can also
check that Argo CD applied exactly the commit of the desired revision.

### D4: Images are pinned by digest

A tag can move to a different image. A digest cannot. The worker resolves
each tag one time and saves the digest in the revision. A rollback copies
the digests. Thus a rollback gives the same images as before, also if
somebody pushed the tag again.

### D5: Transactional outbox and idempotent tasks

The server writes a task into the `outbox` table in the same transaction as
the data change. A relay sends the outbox rows to RabbitMQ with publisher
confirms. Thus a task is never lost and never sent for a change that did
not happen.

The broker can deliver a task more than one time. All task handlers are
idempotent: each handler reads the desired state from the database and
makes the external systems agree with it. A handler that runs two times
gives the same result as a handler that runs one time.

Only one task for each tenant or deployment runs at the same time. The
worker holds a PostgreSQL advisory lock for the object during the task.
This works also with more than one worker replica. Without the lock, a
synchronization that started before a removal could write the files again
after the removal.

### D6: Retries with a delay, then the dead queue

If a task fails, the worker sends it to the retry queue with a delay. The
delay doubles after each attempt, up to two minutes. After the maximum
number of attempts (default 15), the worker records the failure in the
database and moves the task to the dead queue `squared.tasks.dead`. Errors
that a retry cannot repair (for example a bad package) fail immediately.
A user can start the work again with "Process again".

### D7: Periodic reconciliation

Argo CD repairs drift in the cluster. The worker repairs drift in Git:
every 30 minutes (`SQUARED_RECONCILE_INTERVAL`) it processes all ready
tenants and all committed deployments again. Because the tasks are
idempotent, this changes nothing if Git is correct. If somebody changed a
file in a repository by hand, the worker writes the correct file again.

### D8: Server-side HTML without JavaScript

The web interface uses `html/template` and plain HTML forms. This gives a
small attack surface: the Content Security Policy blocks all scripts.
Pages that show work in progress load again every five seconds with
`<meta http-equiv="refresh">`.

### D9: Few dependencies

Squared uses the Go standard library for HTTP, templates, JSON and the
Kubernetes, Forgejo and Harbor clients. It uses only these external
modules:

| Module | Reason |
|---|---|
| `github.com/jackc/pgx/v5` | PostgreSQL protocol. Complex and well tested. |
| `github.com/rabbitmq/amqp091-go` | AMQP protocol. The official RabbitMQ client. |
| `golang.org/x/crypto` | Argon2id password hashes. Security code from the Go team. |
| `github.com/coreos/go-oidc/v3` | OpenID Connect discovery and token verification (uses `go-jose`). |
| `golang.org/x/oauth2` | OAuth 2.0 authorization code flow with PKCE. |

### D10: One binary, configuration from the environment

The same image runs the server, the worker and the migrations. All
configuration comes from environment variables. Secrets come from files
(`*_FILE` variables), so that they do not show in the process environment.

### D11: Keycloak signs users in; Squared keeps the permissions

Keycloak is the identity provider. It keeps the passwords, the second
factors and the account policies. Squared does not store passwords of SSO
users. Keycloak also tells which users are platform administrators (a
group). The roles in tenants stay in Squared, because they are part of the
data of a tenant and owners change them in Squared.

Squared uses the libraries `github.com/coreos/go-oidc/v3` (with
`github.com/go-jose/go-jose/v4`) and `golang.org/x/oauth2`. Token
verification is security code, thus Squared does not implement it itself.

The API accepts two kinds of bearer tokens: personal access tokens of
Squared (`sqp_...`) and access tokens of Keycloak with the audience
`squared-api`. Keycloak tokens are short-lived, thus scripts that can get
tokens from Keycloak do not need a long-lived secret from Squared.

## Data model

```
users ─┬─< memberships >─┬─ tenants ─┬─< environments ─┬─< deployments ─┬─< revisions
       │                 │           │                 │                │
       ├─< sessions      │           └─< audit_events  │                │
       └─< api_tokens    │                             │                │
                         └─ role: owner | developer | viewer
outbox (tasks for the relay)
```

The schema is in `internal/store/migrations`. The server applies the
migrations at start, or the `squared migrate` command applies them.
