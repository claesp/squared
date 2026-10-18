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
| `SQUARED_WORKER_CLUSTER_ROLE` | `squared-worker-pull-secrets` | ClusterRole that lets the worker write the image pull secret. The tenant RoleBindings refer to it. |
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
| `SQUARED_FORGEJO_AUTH_SOURCE_ID` | (necessary with Keycloak) | ID of the Keycloak authentication source in Forgejo (`forgejo admin auth list`). The worker makes the Forgejo accounts of tenant members for this source and their Keycloak user IDs. Forgejo has no API for the sources. |
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
| `argocd-rbac` | `namespace`, `name`, `ignoreFields` | Subjects of the `g` lines in the keys `policy.csv` and `policy.*.csv`. Checks the keys against the keys of Argo CD 3.5, and the claims in `scopes` (see [security.md](security.md)). `ignoreFields` lists new keys that an operator checked. |
| `argocd-repositories` | `url` (the API of Argo CD), `passwordFile` (a token of an account with `repositories, get` and `write-repositories, get`), `repoPrefixes` | No groups. The repositories and credential templates of Argo CD, from its API, without secrets. An error for a template that ends in the host name, for a GitOps repository without the check of TLS certificates or SSH host keys, for write credentials of the GitOps repositories while the source hydrator is on, and if the guard sees no credentials of the GitOps repositories (see [security.md](security.md)). `repoPrefixes` are the addresses of the GitOps repositories, for example `https://git.example/squared/`. |
| `argocd-clusters` | `url` (the API of Argo CD), `passwordFile` (a token of an account with `clusters, get`), `clusterServers` | No groups. The clusters of Argo CD, from its API, without secrets. An error for a cluster in `clusterServers` (the value of `SQUARED_CLUSTER_SERVER`) without the check of TLS certificates or with an `http://` address, or if the guard does not see it; a warning for other such clusters and for a cluster Secret for the own cluster of Argo CD (see [security.md](security.md)). |
| `argocd-projects` | `namespace`, `repoPrefixes`, `clusterServers`, `ignoreFields` | Groups of the roles of all AppProjects, except the tenant groups in the role `member` of their own tenant project, and the subjects of `g` lines in the policies of the roles. Checks the field names against the fields of Argo CD 3.5. `ignoreFields` lists new fields that an operator checked, as `spec.<field>` or `roles.<field>`. An error for a tenant project (name `tenant-<slug>`) that permits more than Squared puts into it: other clusters or namespaces, other repositories, other kinds, other roles or policies (see [security.md](security.md)). |
| `argocd-applications` | `namespace`, `name` (the root application, for example `squared-platform`), `rootSource` (`repoURL`, `targetRevision` and `path` of the root application), `repoPrefixes`, `clusterServers`, `applicationNamespaces` (optional), `controllerBindRoles` (the ClusterRoles that the application controller may bind, for example `squared-worker-pull-secrets`) | No groups. Reads the applications and the ApplicationSets in all namespaces. An error for each value of `application.namespaces` or `applicationsetcontroller.namespaces` of Argo CD, other than its own namespace, that is not in `applicationNamespaces`. An error for an application of Squared that does not sync automatically with `prune` and `selfHeal`, or that differs from what Squared puts into it, also in its source, for an application that Squared did not make in a tenant project or with a source in the GitOps repositories, for a root application without automatic sync, `prune` or `selfHeal`, or with another source than `rootSource`, for an ApplicationSet that can make applications with a tenant project, a GitOps repository or the labels of Squared, that reads a GitOps repository, that sends credentials without TLS checks, that has no `preserveResourcesOnDeletion`, that gives its applications the resources finalizer of Argo CD, that has a template patch with one of these problems or that the guard cannot read, that has a plugin generator whose ConfigMap takes its token from a key that is not a plugin token or sends it without TLS, that has a cluster decision resource generator for a kind of Argo CD, or that makes an application with the name of an application of Squared, for an application of Squared with an owner, for SCM generators of the ApplicationSet controller without a list of HTTPS servers or without the strict mode for token Secrets, and for a policy of the ApplicationSet controller, or of an ApplicationSet, that permits updates of the applications (see [security.md](security.md)). The guard needs `list` on `applications` and `applicationsets` in the cluster, `get` on the ConfigMaps of the namespace of Argo CD (`argocd-cmd-params-cm` and the ConfigMaps of the plugin and cluster decision resource generators), the StatefulSet `argocd-application-controller` and the Deployments `argocd-server` and `argocd-applicationset-controller`, and `list` on the Roles, ClusterRoles and their bindings in the cluster. An error for rights of the ApplicationSet controller that it does not need, for rights of the application controller and of the API server that the projects do not need, for rights or a token of the API for the repository server and for Redis, and for more rights of the job that makes the password of Redis. A warning while the service account of that job has a binding. An error for more rights of Dex than the Role of the chart, and a warning when Dex runs without `dex.config`. The same for the notifications controller, with a warning when it runs without a service in `argocd-notifications-cm`. An error for rights or a token of the API for the commit server, and a warning when it runs while the source hydrator is off (see [security.md](security.md)). The guard also needs `get` on the StatefulSet `argocd-application-controller`, the Deployments `argocd-repo-server` and `argocd-redis`, and their service accounts. |
| `harbor-members` | `url`, `user`, `passwordFile` | Group members of all projects, except the tenant groups in their own tenant project. A robot account with "list projects" and "list members" is enough. |
| `configmap` | `namespace`, `name` or `selector`, `key` | Group paths in one key, separated by commas or white space. |
| `forgejo-database` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_auth_sources`), `ignoreFields` | The administrator, restricted, team map, required claim and quota map groups of all OAuth authentication sources, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). Sees changes in the web interface. Checks the field names against the fields of Forgejo 15 (see [security.md](security.md)). `ignoreFields` lists new fields that an operator checked: they give no rights. |
| `harbor-database` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_harbor_config`), `ignoreFields` | `oidc_admin_group` and `http_authproxy_admin_groups` of the Harbor configuration, from the view of [`harbor-guard-view.sql`](../deploy/kind/harbor-guard-view.sql). Sees changes in the web interface and the API. Checks the keys against the settings of Harbor 2.15 (see [security.md](security.md)). `ignoreFields` lists new keys that an operator checked: they give no rights. |
| `keycloak-mappers` | `clients`, `audiences`, `claims` (default `["groups"]`), `ignoreFields` | No groups. Checks that only group membership mappers with full paths write the group claims, and only the mappers of the user's email address and its verification write `email` and `email_verified`, in the tokens that the services accept: the tokens of `clients`, and of other clients with an audience mapper for one of `clients` or `audiences`. Reads the own mappers and the client scopes of each client (see [security.md](security.md)). `ignoreFields` lists mapper types that an operator checked. Needs the role `view-clients`. |
| `keycloak-realm` | `ignoreFields` | No groups. Checks that a person can get only an email address that the person owns: no duplicate addresses, users cannot change their address, registration only with verification, and no identity provider or user federation with `trustEmail` (see [security.md](security.md)). `ignoreFields` lists identity providers and user federation that an operator checked. Needs `view-identity-providers`. |
| `forgejo-credentials` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_credential_owners`), `sourceId` | No groups. Compares the owners of Forgejo personal access tokens and SSH keys with their Keycloak users, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error if the Keycloak user does not exist or is disabled, or if the owner is a Forgejo administrator but not in the administrator group of the source (see [security.md](security.md)). `sourceId` is the ID of the Keycloak authentication source in Forgejo. Needs `view-users` in Keycloak. |
| `forgejo-deploy-keys` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_deploy_keys`), `org` (default `squared`), `ignoreFields` | No groups. The deploy keys of the repositories of the organization `org`, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error for a key with write access, a warning for a key with read access (see [security.md](security.md)). `ignoreFields` lists keys that an operator checked, as `<repository>/<key name>`. |
| `forgejo-writers` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_repo_access`), `org` (default `squared`), `writers`, `ignoreFields` | No groups. The users with access to the code of the repositories of the organization `org` through teams or as collaborators, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error for write access of a user who is not in `writers` (for example `["squared-bot"]`), a warning for a collaborator with read access (see [security.md](security.md)). `ignoreFields` lists accesses that an operator checked, as `team:<team>/<user>` or `collaborator:<repository>/<user>`. |
| `forgejo-branches` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_branch_rules`), `org` (default `squared`), `writers`, `ignoreFields` | No groups. The protection rule of the default branch of each repository of the organization `org`, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error if a repository has no rule with the name of the branch, or if the rule lets users other than `writers`, teams or deploy keys push or merge, does not apply to administrators, or stops the writers (see [security.md](security.md)). `ignoreFields` lists repositories that an operator checked. |
| `forgejo-webhooks` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_webhooks`), `org` (default `squared`), `webhooks` | No groups. The webhooks of the organization `org` and of its repositories, and the system and default webhooks, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error for a webhook with an address that is not in `webhooks`; a warning if the organization has no active webhook to an address in `webhooks` (see [security.md](security.md)). `webhooks` has addresses, or `sha256:<hex>` (the SHA-256 of the address) for an address with a secret. |
| `forgejo-mirrors` | `url` (PostgreSQL, without password), `passwordFile`, `name` (default `squared_guard_mirrors`), `org` (default `squared`), `ignoreFields` | No groups. The pull and push mirrors of the repositories of the organization `org`, from the view of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql). An error for each (see [security.md](security.md)). `ignoreFields` lists mirrors that an operator checked, as `<repository>/pull` or `<repository>/push`. |
| `forgejo-config` | `url`, `webhookHosts`, `ignoreFields` | No groups. Reads the sign-in and registration settings of the Forgejo `app.ini` from the sidecar `squared forgejo-config` (see below), and checks that accounts come only from Keycloak: no automatic account linking, no local registration, no OpenID 2.0 sign-in or registration, no reverse proxy authentication, no password on the web form, no Forgejo password for Keycloak users, no webhook host in `ALLOWED_HOST_LIST` other than `external` and `webhookHosts`, no migration from local networks or from a pattern of `ALLOWED_DOMAINS` that matches names in the cluster, no import of local paths, no connection without the check of TLS certificates, now and after the next start of Forgejo, no change after the start, and no unknown sign-in key (see [security.md](security.md)). `ignoreFields` lists new keys that an operator checked, as `<section>.<key>`. |
| `forgejo-init` | `namespace`, `name`, `key` (default `configure_gitea.sh`) | `--admin-group`, `--restricted-group` and the keys of `--group-team-map` in the init script of the Forgejo Helm chart. Use it only if the guard cannot reach the Forgejo database: it does not see changes in the web interface. |

All groups are full paths, for example `/squared-admins`. The guard warns
about a short name: it never matches the groups claim.

## `squared forgejo-config`

A sidecar in the pod of Forgejo. Forgejo has no API for its sign-in and
webhook settings, and reads them only from `app.ini` at its start. The
sidecar reads them at its own start, which is the start of the pod, and
on each request, and serves both at `GET /settings`: all of `[admin]`,
`[oauth2_client]` and `[openid]`, the key names of `[service]` with the
values of the sign-in and registration keys only, `INSTALL_LOCK` and
`IMPORT_LOCAL_PATHS` of `[security]`, the key names of `[webhook]` with the values of
`ALLOWED_HOST_LIST` and `SKIP_TLS_VERIFY` only, all of `[migrations]`,
`FORCE_TRUST_SERVER_CERT` and `SKIP_VERIFY` of `[mailer]`,
`SKIP_TLS_VERIFY` of `[email.incoming]`, and `MINIO_INSECURE_SKIP_VERIFY`
of every section that has it. It never serves secrets or other keys. Run it as the
Forgejo user (the file has the mode 0600), with the directory of
`app.ini` mounted read-only, and permit only the guard to connect.
[`deploy/kind/forgejo-values.yaml`](../deploy/kind/forgejo-values.yaml)
and [`deploy/kind/forgejo-config.yaml`](../deploy/kind/forgejo-config.yaml)
are a complete example.

| Variable | Default | Description |
|---|---|---|
| `SQUARED_FORGEJO_APP_INI` | `/data/gitea/conf/app.ini` | Path of `app.ini`. |
| `SQUARED_LISTEN` | `:8083` | Address of the HTTP server. |
