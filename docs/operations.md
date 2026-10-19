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
   same secret in Argo CD (`webhook.gogs.secret` in `argocd-secret`). Set
   random secrets for the other providers too (`configs.secret` of the
   Helm chart): without a secret, the webhooks of Argo CD accept the events
   of that provider without a signature. Restart the ApplicationSet
   controller after a change of these secrets. Turn on the metrics of the
   ApplicationSet controller (`applicationSet.metrics.enabled`), thus the
   chart makes a NetworkPolicy that closes its webhook.
3. Add a repository credential template in Argo CD for the address of the
   organization, with the Argo CD account.
4. Apply the root application. Use
   [`deploy/kind/argocd-platform.yaml`](../deploy/kind/argocd-platform.yaml)
   as the template and change the repository address. It also locks the
   project `default` of Argo CD.
5. Limit the application controller and the API server of Argo CD to the
   kinds that the projects permit: `controller.clusterRoleRules` and
   `server.clusterRoleRules` of the Helm chart, and
   `resource.respectRBAC: normal` in `argocd-cm`. Turn off the token of the
   API for the repository server (`repoServer.automountServiceAccountToken`
   and `repoServer.serviceAccount.automountServiceAccountToken`) and for
   Redis, give Redis its own service account, and make the Secret
   `argocd-redis` with the key `auth` yourself, with
   `redisSecretInit.enabled: false` and `redis.existingSecret:
   argocd-redis`. In an existing installation, the job made this Secret
   already: keep it, else Redis and the other components of Argo CD get
   different passwords. Turn off Dex (`dex.enabled: false`) when Argo CD
   signs in with Keycloak directly, and the notifications controller
   (`notifications.enabled: false`) when Squared sends no notifications. Use
   [`deploy/kind/argocd-values.yaml`](../deploy/kind/argocd-values.yaml)
   as the template. The chart defaults give them rights on all kinds in
   the cluster, and the guard reports this.
6. Configure Keycloak. See the next section.
7. Make the secrets `squared-server` and `squared-worker` in the namespace
   `squared`. See [`hack/kind-up.sh`](../hack/kind-up.sh) for the keys.
8. Make an overlay of [`deploy/kubernetes/base`](../deploy/kubernetes/base)
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
   **full group path on**, in the ID token and the access token. All
   services compare full paths, for example `/squared-admins`.
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
   accounts" on. Give its service account only the client roles
   `query-users` and `query-groups` of `realm-management`.
2. Turn on **Admin Permissions** in the realm settings (Keycloak 26.2 or
   later). Then, in **Permissions**, make:
   - a user policy `squared-worker` for the service account user;
   - a realm role `squared-protected-group-admin` that nobody has, and a
     role policy for it;
   - a permission for all users: `view`, `manage-group-membership`,
     policy `squared-worker`;
   - a permission for all groups: `view`, `view-members`, `manage`,
     `manage-membership`, policy `squared-worker` (the guard adds its
     policy later, see below);
   - a permission for the specific groups that give rights (for example
     `squared-admins`), with the scopes `view`, `manage` and
     `manage-membership`, and only the role policy. Do not add the member
     scopes: they would block the tenant memberships of administrators.

   The function `setup_keycloak_permissions` in
   [`hack/kind-up.sh`](../hack/kind-up.sh) does the same through the admin
   API. See [security.md](security.md) for the reasons.
3. Set the `SQUARED_KEYCLOAK_*` variables of the worker.
4. In Forgejo, make the bot of the worker an administrator and give its
   token the scope `write:admin`. In `[oauth2_client]`, set
   `ACCOUNT_LINKING = disabled` and `USERNAME = nickname`. In `[service]`,
   set `ENABLE_INTERNAL_SIGNIN = false`; in `[admin]`, set
   `EXTERNAL_USER_DISABLE_FEATURES = manage_password`. The local
   administrator then uses only the API and the command line in the pod.
   In `[webhook]`, set `ALLOWED_HOST_LIST` to `external` and the host of
   Argo CD only, and put that host into `webhookHosts` of the guard
   source `forgejo-config`. Set
   `SQUARED_FORGEJO_AUTH_SOURCE_ID` of the worker to the ID of the Keycloak
   authentication source (`forgejo admin auth list`).
5. If Forgejo has accounts from an earlier version of Squared, which
   Forgejo linked by the email address, give each of them its Keycloak
   user ID: `PATCH /api/v1/admin/users/<name>` with `source_id` and
   `login_name` (the Keycloak user ID). Check each account by hand. Until
   then, the access sync of the tenant fails with "the email address
   belongs to another account".

The files in [`deploy/kind`](../deploy/kind) and the functions
`setup_harbor` and `install_charts` in
[`hack/kind-up.sh`](../hack/kind-up.sh) are complete examples. Each
service must reach the issuer address of Keycloak, the same address as
the browser uses.

If Keycloak is not available, users cannot sign in, but existing sessions
continue. The sign-in page then tells the user to try again later.

## The guard

The guard (decision D14 in the architecture) keeps the Keycloak
permission `protected-groups` up to date. To install it in production:

1. Make a confidential client `squared-guard` with only "service
   accounts" on. Give its service account the client roles `manage-realm`,
   `view-users`, `query-groups`, `view-clients` and
   `view-identity-providers` of `realm-management`. With `view-clients`
   the guard reads the client mappers and finds the `admin-permissions`
   client; with `view-identity-providers` it reads the email trust of the
   identity providers. Keycloak gives it no secrets. In the user profile
   of the realm, permit only `admin` to edit the attribute `email`.
2. Make a user policy for this service account, and an aggregated policy
   with the decision strategy "Affirmative" that contains the policies of
   the worker and of the guard. Use the aggregated policy in the permission
   for all groups, instead of the policy of the worker. The guard can then
   make missing groups. Do not make a second permission for all groups:
   Keycloak grants a scope only if all permissions for the same resources
   grant it, and admin permissions accept only that strategy, thus two such
   permissions block each other.
3. If Forgejo uses PostgreSQL, run
   [`deploy/kind/forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql)
   as the owner of the Forgejo tables, after Forgejo made them, and set a
   password for the role `squared_guard`. Permit connections from the guard
   to the database. Before a Forgejo upgrade that changes a table that the
   views read, drop the views; run the file
   again after the upgrade.
   Without database access, use the source type `forgejo-init`. Add the
   sidecar `squared forgejo-config` to the Forgejo pod, a Service for it,
   and a network policy that permits only the guard to connect (see
   [configuration.md](configuration.md)), and a source `forgejo-config`.
4. In Argo CD, add the account `squared-guard` with only the capability
   `apiKey` (`accounts.squared-guard: apiKey` in `argocd-cm`) and the
   lines `p, squared-guard, repositories, get, *, allow` and
   `p, squared-guard, write-repositories, get, *, allow` and
   `p, squared-guard, clusters, get, *, allow` in the RBAC
   policy, make a token for it, and put the token into the secret of the
   guard. See `setup_argocd_guard` in
   [`hack/kind-up.sh`](../hack/kind-up.sh).
5. Make a Harbor robot account at system level with the permissions "list
   projects" and "list members" of all projects. Run
   [`deploy/kind/harbor-guard-view.sql`](../deploy/kind/harbor-guard-view.sql)
   in the Harbor database (`registry`) as the owner of the Harbor tables,
   and set a password for the role `squared_guard`. Permit connections from
   the guard to the database. Before a Harbor upgrade that changes the
   table `properties`, drop the view; run the file again after the upgrade.
6. Write the configuration file (see [configuration.md](configuration.md))
   with a source for each service that gives rights to groups.
7. Give the service account of the guard read access to the objects of
   the sources (see
   [`deploy/kubernetes/kind/guard-rbac.yaml`](../deploy/kubernetes/kind/guard-rbac.yaml)).
8. Run one replica. `GET /status` on port 8082 shows the protected groups,
   the places that refer to them, the errors and the warnings of the last
   run.

When you add a service that gives rights to groups, add a source for it
to the configuration of the guard, and add its Keycloak client to
`clients` of the source `keycloak-mappers`.

### Forgejo upgrades

1. Drop the views `squared_guard_auth_sources`,
   `squared_guard_credential_owners`, `squared_guard_deploy_keys`,
   `squared_guard_repo_access`, `squared_guard_branch_rules`,
   `squared_guard_webhooks` and `squared_guard_mirrors` if the upgrade
   changes the tables that they
   read (`login_source`, `user`, `access_token`, `public_key`,
   `deploy_key`, `repository`, `team`, `team_user`, `team_unit`,
   `team_repo`, `collaboration`, `protected_branch`, `webhook`, `mirror`
   or `push_mirror`). After the upgrade, run
   `forgejo-guard-view.sql` again.
2. Look at `GET /status` of the guard:
   - An error about a missing or new group field: Forgejo renamed or
     added a field that can give rights. The guard keeps the current
     protection. Update the list of fields in
     [`internal/guard/forgejo.go`](../internal/guard/forgejo.go) and the
     view, and release a new version of Squared.
   - A warning about a new field without group meaning: check the Forgejo
     release notes. If the field gives no rights, add it to
     `ignoreFields` of the source.
   - An error or a warning about an unknown key in `[oauth2_client]`,
     `[openid]` or `[service]`: Forgejo added or renamed a setting. Check
     that it cannot make accounts or link accounts without Keycloak, then
     add `<section>.<key>` to `ignoreFields` of the source
     `forgejo-config`, or update the list in
     [`internal/guard/forgejoconfig.go`](../internal/guard/forgejoconfig.go).
3. Run the end-to-end test against a test environment with the new
   version.

### Keycloak upgrades

1. Look at `GET /status` of the guard after the upgrade. An error about an
   unknown mapper type: Keycloak added a type, or an extension has one.
   Check what the type writes. If it cannot write a group claim, add it to
   `ignoreFields` of the source `keycloak-mappers`, or update the list in
   [`internal/guard/keycloak.go`](../internal/guard/keycloak.go) and
   release a new version of Squared.
2. Run the end-to-end test against a test environment with the new
   version.

### Argo CD upgrades

1. Look at `GET /status` of the guard after the upgrade:
   - An error about a new key in `argocd-rbac-cm` or a new project field
     that can give rights: the guard keeps the current protection. Check
     the Argo CD release notes. Update the lists in
     [`internal/guard/argocd.go`](../internal/guard/argocd.go), and
     release a new version of Squared. If the key or field gives no rights
     to groups, you can also add it to `ignoreFields` of the source.
   - A warning about a new project field: check the release notes. If the
     field gives no rights, add it to `ignoreFields`.
   - An error about rights of the application controller or of the
     ApplicationSet controller: a new chart version can have new rules.
     Compare them with the rules in
     [`deploy/kind/argocd-values.yaml`](../deploy/kind/argocd-values.yaml)
     and with the Roles in
     [`internal/guard/argocdrbac.go`](../internal/guard/argocdrbac.go)
     and [`internal/guard/argocdappcontroller.go`](../internal/guard/argocdappcontroller.go).
2. Run the end-to-end test against a test environment with the new
   version.

### Harbor upgrades

1. Drop the view `squared_guard_harbor_config` if the upgrade changes the
   table `properties`. After the upgrade, run `harbor-guard-view.sql`
   again.
2. Look at `GET /status` of the guard:
   - An error about a new setting that can give rights: Harbor renamed or
     added a setting. The guard keeps the current protection. Update the
     list of settings in
     [`internal/guard/harbor.go`](../internal/guard/harbor.go) and the
     view, and release a new version of Squared.
   - A warning about a new setting: check the Harbor release notes. If the
     setting gives no rights, add it to `ignoreFields` of the source.
3. Run the end-to-end test against a test environment with the new
   version.

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
| `squared_guard_runs_total` | guard | `result` | Runs of the guard. `result` is `ok` or `error`. |
| `squared_guard_changes_total` | guard | `change` | Groups that became protected (`protected`) or not protected (`released`). |
| `squared_guard_warnings_total` | guard | `kind` | `members`: a new protected group has members. `short-name`: a configuration uses a short group name. `source`: a source reports a change to check, for example a new Forgejo field, Harbor setting or Argo CD project field. |
| `squared_guard_source_errors_total` | guard | `source` | Runs in which a source failed. |

Useful alerts:

- `increase(squared_tasks_total{result="dead"}[15m]) > 0`: a task failed
  permanently.
- `increase(squared_outbox_errors_total[10m]) > 5`: the server cannot send
  tasks to RabbitMQ.
- `sum(rate(squared_http_requests_total{status="5xx"}[5m])) > 0`: internal
  errors.
- `increase(squared_guard_runs_total{result="error"}[5m]) > 0`: the guard
  cannot read a source or update the permission.
- `increase(squared_guard_warnings_total{kind="members"}[1h]) > 0`: a group
  became protected with members; check them.
- `increase(squared_guard_source_errors_total[15m]) > 0`: a source of the
  guard failed, for example because a Forgejo, Harbor or Argo CD upgrade changed the fields of
  the authentication sources. `GET /status` tells which field.

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

1. Read the release notes. From this version, the tenant namespaces have
   no Role `squared-worker`: the RoleBinding refers to the ClusterRole
   `squared-worker-pull-secrets` in
   [`deploy/kubernetes/base`](../deploy/kubernetes/base). The platform
   project does not permit Roles, thus Argo CD does not remove the old
   Roles. Remove them after the upgrade:
   `kubectl delete role squared-worker -A -l app.kubernetes.io/managed-by=squared`.
2. Apply the new image. The init container of the server applies new
   database migrations. Migrations only add to the schema, thus the old
   version continues to work during the rollout.
3. The worker stops gracefully: it gives tasks in progress back to
   RabbitMQ.
