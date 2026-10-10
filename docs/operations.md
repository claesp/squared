# Operations

This document tells operators how to install, monitor and repair Squared
in a production cluster.

## Requirements

| System | Notes |
|---|---|
| Kubernetes | A version that Argo CD supports. A CNI with NetworkPolicy support. |
| Argo CD | Installed in the namespace `argocd` (or set `SQUARED_ARGOCD_NAMESPACE`). |
| Forgejo | One organization for Squared (default `squared`). |
| Harbor | Version 2. The nodes must be able to pull from it. |
| PostgreSQL | Version 15 or later. |
| RabbitMQ | Version 3.13 or later (quorum queues). Three nodes for production. |
| Keycloak (optional) | Or another OpenID Connect provider with a groups claim. |

## Install

1. Make the Forgejo organization and two accounts:
   - a worker account that is an owner of the organization, with a token
     that has the scopes `write:organization`, `write:repository` and
     `read:user`;
   - an Argo CD account in a team with read access to all repositories of
     the organization, with a token that has the scope `read:repository`.
2. Add an organization webhook in Forgejo: type "Gitea", event "push",
   address `https://<argocd-server>/api/webhook`. Set a secret, and set the
   same secret in Argo CD (`webhook.gogs.secret` in `argocd-secret`).
3. Add a repository credential template in Argo CD for the address of the
   organization, with the Argo CD account.
4. Apply the root application. Use
   [`deploy/kind/argocd-platform.yaml`](../deploy/kind/argocd-platform.yaml)
   as the template and change the repository address.
5. Configure Keycloak. See the next section.
6. Make the secrets `squared-server` and `squared-worker` in the namespace
   `squared`. See [`hack/kind-up.sh`](../hack/kind-up.sh) for the keys.
7. Make an overlay of [`deploy/kubernetes/base`](../deploy/kubernetes/base)
   with your configuration and image, and apply it. The Kind overlay is an
   example. In production, Argo CD can apply Squared itself.

## Keycloak

Make these objects in the realm of your organization. The development
realm file
[`deploy/kind/keycloak/realm-squared.json`](../deploy/kind/keycloak/realm-squared.json)
is a complete example.

1. A client `squared`:
   - client authentication on (confidential), standard flow only;
   - valid redirect URI `https://<squared>/auth/callback`;
   - valid post logout redirect URI `https://<squared>/login`;
   - PKCE method `S256` (advanced settings).
2. A protocol mapper "Group Membership" on the client: claim name `groups`,
   full group path off, in the ID token and the access token.
3. A protocol mapper "Audience" on the client: included custom audience
   `squared-api`, in the access token. Add the same mapper to other clients
   whose tokens may call the Squared API.
4. A group `squared-admins` for the platform administrators.
5. Verified email addresses for all users. Squared refuses a first
   sign-in without a verified email address.

Then set the `SQUARED_OIDC_*` variables (see
[configuration.md](configuration.md)) and put the client secret in the
secret of the server.

### Argo CD, Harbor and Forgejo

Make one confidential client for each service, each with the groups
mapper of step 2. Then configure the services:

| Service | Redirect URI | Configuration |
|---|---|---|
| Argo CD | `https://<argocd>/auth/callback` (and `http://localhost:8085/auth/callback` for the CLI) | `oidc.config` in `argocd-cm`; the client secret in `argocd-secret` (`oidc.keycloak.clientSecret`); `g, squared-admins, role:admin` in `argocd-rbac-cm`. |
| Harbor | `https://<harbor>/c/oidc/callback` | **Configuration > Authentication** or `PUT /api/v2.0/configurations` with `auth_mode: oidc_auth`, `oidc_groups_claim: groups`, `oidc_admin_group: squared-admins`. Harbor permits this change only before users other than `admin` exist. Add the scope `offline_access` to the client, for the CLI secrets of users. |
| Forgejo | `https://<forgejo>/user/oauth2/keycloak/callback` | An authentication source of type OpenID Connect with group claim `groups` and administrator group `squared-admins`. Set `ALLOW_ONLY_EXTERNAL_REGISTRATION=true` and `ENABLE_AUTO_REGISTRATION=true`. |

For the copy of tenant members (decision D13 in the architecture):

1. Make a confidential client `squared-worker` with only "service
   accounts" on. Give its service account the client roles
   `manage-users`, `view-users`, `query-users` and `query-groups` of
   `realm-management`.
2. Set the `SQUARED_KEYCLOAK_*` variables of the worker.
3. In Forgejo, make the bot of the worker an administrator and give its
   token the scope `write:admin`. Set `ACCOUNT_LINKING = auto` in
   `[oauth2_client]`.

The files in [`deploy/kind`](../deploy/kind) and the functions
`setup_harbor` and `install_charts` in
[`hack/kind-up.sh`](../hack/kind-up.sh) are complete examples. Each
service must reach the issuer address of Keycloak, the same address as
the browser uses.

If Keycloak is not available, users cannot sign in, but existing sessions
continue. The sign-in page then tells the user to try again later.

## Health checks

| Role | Path | Meaning |
|---|---|---|
| server | `GET /healthz` on port 8080 | The process runs. |
| server | `GET /readyz` on port 8080 | The database answers. |
| worker | `GET /healthz` on port 8081 | The process runs. |
| worker | `GET /readyz` on port 8081 | The database answers. |

## Metrics

Both roles serve metrics in the Prometheus text format at `GET /metrics`:
the server on port 9090, the worker on port 8081. The network policy of the
namespace `squared` blocks these ports. Add a policy that permits your
monitoring system.

| Metric | Role | Labels | Meaning |
|---|---|---|---|
| `squared_http_requests_total` | server | `method`, `route`, `status` | HTTP requests. `status` is the class, for example `2xx`. |
| `squared_http_request_seconds` | server | `route` | Duration of HTTP requests (sum and count). |
| `squared_outbox_published_total` | server | | Tasks that the relay sent to RabbitMQ. |
| `squared_outbox_errors_total` | server | | Relay runs that failed. |
| `squared_tasks_total` | worker | `kind`, `result` | Processed tasks. `result` is `done`, `retry` or `dead`. |
| `squared_task_seconds` | worker | `kind` | Duration of task handlers (sum and count). |

Useful alerts:

- `increase(squared_tasks_total{result="dead"}[15m]) > 0`: a task failed
  permanently.
- `increase(squared_outbox_errors_total[10m]) > 5`: the server cannot send
  tasks to RabbitMQ.
- `sum(rate(squared_http_requests_total{status="5xx"}[5m])) > 0`: internal
  errors.

## Logs

Squared writes JSON logs to standard error. Important messages:

| Message | Meaning |
|---|---|
| `request` | One HTTP request: method, path, status, duration. |
| `audit` | A change by a user. The `audit_events` table has the same data. |
| `task done` | The worker completed a task. |
| `task failed; retry later` | A task failed. The worker tries again later. |
| `task failed; no more retries` | The worker gave up. The task is in the dead queue. |
| `relay outbox` (error) | The server cannot send tasks to RabbitMQ. The tasks stay in the database. |
| `sign-in rate limit` | Too many sign-in attempts from one address or for one account. |
| `signed in` | A user signed in. `method` is `sso` or `password`. |
| `complete SSO sign-in` (error) | The token exchange or the token verification failed. |
| `SSO callback with wrong state` | A callback that does not agree with its sign-in attempt. Possibly an attack. |

## Failure modes

| Failure | Effect | Recovery |
|---|---|---|
| RabbitMQ is not available | Users can still make changes. Tasks wait in the `outbox` table. | Automatic when RabbitMQ is available again. |
| Worker stops | Tasks wait in RabbitMQ. Status does not change. | Automatic when the worker starts again. |
| Forgejo, Harbor or Kubernetes is not available | Tasks fail and the worker tries again, with longer delays. | Automatic, if the problem stops before the last attempt. Else select "Process again". |
| Argo CD stops | New commits are not applied. Running applications continue. | Automatic when Argo CD starts again. |
| Image not found | The deployment fails after three attempts. | Push the image, then select "Process again". |

## Dead queue

Tasks that fail permanently go to the queue `squared.tasks.dead`. The
database also records the failure, thus the web interface shows it. A
user starts the work again from the web interface. You can then delete the
messages of the dead queue. Look at them with the RabbitMQ management
interface or with:

```bash
rabbitmqadmin get queue=squared.tasks.dead count=10
```

## Remove an environment namespace

When an owner removes an environment, Squared removes its files from the
platform repository. Argo CD does not delete the namespace, because the
namespace has the annotation `argocd.argoproj.io/sync-options: Prune=false`.
This protects against data loss after a bad commit. Check that the
namespace is empty, then delete it:

```bash
kubectl get all -n <tenant>-<environment>
kubectl delete namespace <tenant>-<environment>
```

## Backups

Back up these systems together:

- **PostgreSQL**: users, tenants, revisions, audit events.
- **Forgejo**: the platform and tenant repositories.
- **Harbor**: the images.

After a restore of PostgreSQL to an earlier time, select "Prepare the
tenant again" for each tenant and "Process again" for each deployment.
This makes Git agree with the database again.

## Upgrade

1. Read the release notes.
2. Apply the new image. The init container of the server applies new
   database migrations. Migrations only add to the schema, thus the old
   version continues to work during the rollout.
3. The worker stops gracefully: it gives tasks in progress back to
   RabbitMQ.
