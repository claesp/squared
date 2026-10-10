# Configuration

Squared reads its configuration from environment variables. For a secret
variable `NAME`, you can set `NAME_FILE` to the path of a file that
contains the value. Use `NAME_FILE` in Kubernetes: mount the secret as a
file. If a variable is not valid, the program stops and lists all problems.

## All roles

| Variable | Default | Description |
|---|---|---|
| `SQUARED_DATABASE_URL` (secret) | (necessary) | PostgreSQL URL, for example `postgres://user:password@host:5432/squared?sslmode=verify-full`. |
| `SQUARED_AMQP_URL` (secret) | (necessary) | RabbitMQ URL, for example `amqps://user:password@host:5671/`. Not used by `migrate`. |
| `SQUARED_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `SQUARED_LOG_FORMAT` | `json` | `json` or `text`. |

## `squared serve`

| Variable | Default | Description |
|---|---|---|
| `SQUARED_LISTEN` | `:8080` | Address of the HTTP server. |
| `SQUARED_METRICS_LISTEN` | `:9090` | Address of the metrics server (`GET /metrics`). |
| `SQUARED_CATALOG_DIR` | `/etc/squared/catalog` | Directory with the package definitions. |
| `SQUARED_MIGRATE` | `true` | Apply database migrations at start. |
| `SQUARED_REGISTRY_HOST` | (necessary) | Host name of Harbor, as users and nodes see it. Used in instructions. |
| `SQUARED_PROJECT_PREFIX` | empty | Prefix of the Harbor project of a tenant. The project is `<prefix><tenant>`. Must be the same as for the worker. |
| `SQUARED_GIT_PUBLIC_URL` | empty | Address of Forgejo for links in the web interface. No links if empty. |
| `SQUARED_GIT_ORG` | `squared` | Forgejo organization of the repositories. |
| `SQUARED_COOKIE_SECURE` | `true` | Set the `Secure` attribute on cookies. Browsers accept secure cookies also on `http://localhost`. |
| `SQUARED_HSTS` | `false` | Send `Strict-Transport-Security`. Set it to `true` if all access uses HTTPS. |
| `SQUARED_TRUST_PROXY` | `false` | Use the last address in `X-Forwarded-For` as the client address for rate limits. Set it only behind a proxy that sets the header. |
| `SQUARED_SESSION_TTL` | `12h` | Time until a session expires. |
| `SQUARED_DEFAULT_QUOTA_CPU` | `4` | Default CPU quota of each environment of a new tenant. |
| `SQUARED_DEFAULT_QUOTA_MEMORY` | `8Gi` | Default memory quota. |
| `SQUARED_DEFAULT_QUOTA_PODS` | `50` | Default pod quota. |
| `SQUARED_BOOTSTRAP_ADMIN_EMAIL` | empty | If set and the database has no users, the server adds this platform administrator at start. Not necessary with OIDC and an administrator group. |
| `SQUARED_BOOTSTRAP_ADMIN_PASSWORD` (secret) | empty | Password of the first administrator. Necessary if the email is set. |
| `SQUARED_LOCAL_LOGIN` | `true` without OIDC, `false` with OIDC | Permit the sign-in with an email address and a password. With OIDC, set it to `true` only for an emergency account. |

### Sign-in with OpenID Connect (for example Keycloak)

OIDC is enabled when `SQUARED_OIDC_ISSUER` is set.

| Variable | Default | Description |
|---|---|---|
| `SQUARED_OIDC_ISSUER` | empty | Issuer URL, exactly as in the `iss` claim of the tokens, for example `https://id.example.com/realms/squared`. |
| `SQUARED_OIDC_DISCOVERY_URL` | the issuer | Base URL for discovery if the server cannot use the issuer URL, for example the address of the Keycloak service inside the cluster. The issuer of the tokens must still be `SQUARED_OIDC_ISSUER`. |
| `SQUARED_OIDC_CLIENT_ID` | (necessary with OIDC) | Client ID of Squared at the provider. |
| `SQUARED_OIDC_CLIENT_SECRET` (secret) | (necessary with OIDC) | Client secret. |
| `SQUARED_OIDC_REDIRECT_URL` | (necessary with OIDC) | `https://<squared>/auth/callback`. Register it at the provider. |
| `SQUARED_OIDC_POST_LOGOUT_URL` | empty | Where the provider sends the browser after sign-out, for example `https://<squared>/login`. Register it at the provider. |
| `SQUARED_OIDC_NAME` | `Keycloak` | Name of the provider on the sign-in button. |
| `SQUARED_OIDC_GROUPS_CLAIM` | `groups` | Claim with the group names. |
| `SQUARED_OIDC_ADMIN_GROUP` | empty | Members of this group are platform administrators. Give the exact value of the groups claim, which must be the full path, for example `/squared-admins`. Squared sets the flag at each sign-in. If empty, administrators set the flag in Squared. |
| `SQUARED_OIDC_API_AUDIENCE` | empty | If set, the API also accepts access tokens of the provider with this value in the `aud` claim. |

## `squared worker`

| Variable | Default | Description |
|---|---|---|
| `SQUARED_HEALTH_LISTEN` | `:8081` | Address for `/healthz`, `/readyz` and `/metrics`. |
| `SQUARED_FORGEJO_URL` | (necessary) | Address of Forgejo for the API. |
| `SQUARED_FORGEJO_TOKEN` (secret) | (necessary) | Forgejo token of an owner of the organization. Scopes: `write:organization`, `write:repository`, `read:user`. |
| `SQUARED_GIT_ORG` | `squared` | Forgejo organization of the repositories. |
| `SQUARED_PLATFORM_REPO` | `platform` | Name of the platform repository. |
| `SQUARED_GIT_BRANCH` | `main` | Branch for commits. |
| `SQUARED_ARGOCD_REPO_URL` | value of `SQUARED_FORGEJO_URL` | Address of Forgejo as Argo CD sees it. Used in Argo CD objects. |
| `SQUARED_ARGOCD_NAMESPACE` | `argocd` | Namespace of Argo CD. |
| `SQUARED_CLUSTER_SERVER` | `https://kubernetes.default.svc` | Destination cluster of the Argo CD applications. |
| `SQUARED_WORKER_SERVICE_ACCOUNT` | `squared-worker` | Service account of the worker. The tenant RoleBindings refer to it. |
| `SQUARED_WORKER_NAMESPACE` | value of `POD_NAMESPACE`, else `squared` | Namespace of the worker. |
| `SQUARED_INGRESS_NAMESPACE` | empty | Namespace of the ingress controller. Its pods can connect to tenant pods. |
| `SQUARED_INGRESS_CLASS` | empty | Ingress class for `web` components with a host. |
| `SQUARED_INGRESS_TLS` | `false` | Add a TLS section to each Ingress (secret `<deployment>-<component>-tls`). |
| `SQUARED_HARBOR_URL` | (necessary) | Address of Harbor for the API. |
| `SQUARED_HARBOR_USER` | `admin` | Harbor user. |
| `SQUARED_HARBOR_PASSWORD` (secret) | (necessary) | Harbor password. |
| `SQUARED_REGISTRY_HOST` | (necessary) | Host name of Harbor in image references. The nodes must be able to pull from it. |
| `SQUARED_PROJECT_PREFIX` | empty | Prefix of the Harbor project of a tenant. |
| `SQUARED_PLATFORM_PROJECT` | `platform` | Public Harbor project for images that all tenants can use. Empty: none. |
| `SQUARED_KEYCLOAK_URL` | empty | Base address of Keycloak, for example `https://id.example.com`. If set, the worker copies tenant members to Keycloak groups, Harbor project members and Forgejo teams. |
| `SQUARED_KEYCLOAK_REALM` | (necessary with Keycloak) | Realm of the groups. |
| `SQUARED_KEYCLOAK_CLIENT_ID` | (necessary with Keycloak) | Client with a service account (client credentials grant). |
| `SQUARED_KEYCLOAK_CLIENT_SECRET` (secret) | (necessary with Keycloak) | Secret of that client. |
| `SQUARED_TENANT_GROUP_PREFIX` | `tenant-` | Start of the group names `<prefix><tenant>-<role>` and of the Forgejo team names `<prefix><tenant>`. |
| `SQUARED_WORKER_CONCURRENCY` | `4` | Number of tasks in progress at the same time. |
| `SQUARED_WORKER_MAX_ATTEMPTS` | `15` | Attempts before a task goes to the dead queue. |
| `SQUARED_STATUS_INTERVAL` | `10s` | Time between two reads of the Argo CD status. |
| `SQUARED_RECONCILE_INTERVAL` | `30m` | Time between two complete synchronizations of all tenants and deployments. They repair manual changes in Git. |
| `SQUARED_KUBE_API_URL` | empty | Kubernetes API address outside a cluster. In a pod, the worker uses the service account. |
| `SQUARED_KUBE_TOKEN_FILE` | | Token file, if `SQUARED_KUBE_API_URL` is set. |
| `SQUARED_KUBE_CA_FILE` | | CA certificate file, if `SQUARED_KUBE_API_URL` is set. |

## Example

The Kind overlay in
[`deploy/kubernetes/kind/kustomization.yaml`](../deploy/kubernetes/kind/kustomization.yaml)
is a complete example.

## `squared guard`

| Variable | Default | Description |
|---|---|---|
| `SQUARED_GUARD_CONFIG_FILE` | `/etc/squared/guard/guard.json` | The configuration file of the guard (see below). |
| `SQUARED_GUARD_INTERVAL` | `30s` | Time between two runs. |
| `SQUARED_HEALTH_LISTEN` | `:8082` | Address for `/healthz`, `/status` and `/metrics`. |
| `SQUARED_KEYCLOAK_URL` | (necessary) | Base address of Keycloak. |
| `SQUARED_KEYCLOAK_REALM` | (necessary) | Realm of the groups. |
| `SQUARED_KEYCLOAK_CLIENT_ID` | (necessary) | Client of the guard, with a service account. |
| `SQUARED_KEYCLOAK_CLIENT_SECRET` (secret) | (necessary) | Secret of that client. |
| `SQUARED_KUBE_API_URL`, `SQUARED_KUBE_TOKEN_FILE`, `SQUARED_KUBE_CA_FILE` | | As for the worker. In a pod, the guard uses its service account. |

The configuration file is JSON. The guard rejects unknown fields.
[`deploy/kubernetes/kind/guard.json`](../deploy/kubernetes/kind/guard.json)
is a complete example.

| Field | Description |
|---|---|
| `permission` | The Keycloak scope permission with the protected groups. Default `protected-groups`. |
| `groups` | Groups that are protected in all cases. At least one is necessary, for example `/squared-admins`. |
| `tenantGroupPrefix` | The same as `SQUARED_TENANT_GROUP_PREFIX` of the worker. |
| `projectPrefix` | The same as `SQUARED_PROJECT_PREFIX` of the worker. |
| `sources` | The places to read. Each source has a `type`, a `service` name for messages, and the fields of its type. |

| `type` | Fields | Groups |
|---|---|---|
| `argocd-rbac` | `namespace`, `name` | Subjects of the `g` lines in the keys `policy.csv` and `policy.*.csv`. |
| `argocd-projects` | `namespace` | Groups of the roles of all AppProjects, except the tenant groups in the role `member` of their own tenant project. |
| `harbor-members` | `url`, `user`, `passwordFile` | Group members of all projects, except the tenant groups in their own tenant project. A robot account with "list projects" and "list members" is enough. |
| `configmap` | `namespace`, `name` or `selector`, `key` | Group paths in one key, separated by commas or white space. |
| `forgejo-database` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_auth_sources`) | The administrator, restricted and team map groups of all OAuth authentication sources, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). Sees changes in the web interface. |
| `forgejo-init` | `namespace`, `name`, `key` (default `configure_gitea.sh`) | `--admin-group`, `--restricted-group` and the keys of `--group-team-map` in the init script of the Forgejo Helm chart. Use it only if the guard cannot reach the Forgejo database: it does not see changes in the web interface. |

All groups are full paths, for example `/squared-admins`. The guard warns
about a short name: it never matches the groups claim.
