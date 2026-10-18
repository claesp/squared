# Development

This document tells you how to build, test and run Squared on your
computer.

## Tools

| Tool | Version | Use |
|---|---|---|
| Go | 1.27 (the `toolchain` line in `go.mod` selects the exact version) | Build, test, and the sign-in helper of the end-to-end test |
| Podman | 5 or later | Images and the Kind node |
| Kind | 0.33 or later | Local Kubernetes cluster |
| kubectl | same minor version as the cluster | Cluster access |
| Helm | 3 or 4 | Argo CD, Forgejo and Harbor |
| curl, jq, openssl | any | Scripts |

The scripts use Podman as the Kind provider
(`KIND_EXPERIMENTAL_PROVIDER=podman`). With rootless Podman, Kind needs
delegated cgroup controllers. The scripts start Kind in a systemd scope
with `Delegate=yes` for this. The user service of systemd must have
`Delegate=yes` (see the
[Kind rootless guide](https://kind.sigs.k8s.io/docs/user/rootless/)).

Use about 9 GB of free memory and 20 GB of free disk for the complete
environment.

## Layout

```
cmd/squared/            main package: serve, worker, guard, migrate, check-catalog
internal/domain/        types, validation rules, errors
internal/store/         PostgreSQL access and migrations
internal/core/          business rules and permission checks
internal/catalog/       package definitions and parameter values
internal/render/        Kubernetes manifests from components
internal/gitops/        files of the platform repository
internal/web/           web interface (templates, CSS, handlers)
internal/api/           JSON API
internal/queue/         RabbitMQ publisher, consumer and outbox relay
internal/worker/        task handlers and Argo CD status watcher
internal/guard/         protection of the groups that give rights in services
internal/keycloak/      Keycloak admin client (groups, permissions)
internal/forgejo/       Forgejo client
internal/harbor/        Harbor client
internal/kube/          minimal Kubernetes client
internal/rest/          JSON over HTTP helper
internal/httpx/         HTTP middleware
internal/auth/          password hashes and tokens
internal/sso/           OpenID Connect client (sign-in, access tokens)
internal/config/        environment variables
catalog/                default package catalog
deploy/kubernetes/      manifests of Squared (kustomize base and Kind overlay)
deploy/kind/            Kind cluster, Helm values and Keycloak for development
hack/                   development scripts
examples/hello/         example application for tests
```

## Build and test

```bash
make build      # bin/squared
make check      # gofmt, go vet, catalog check
make test       # unit tests; tests that need PostgreSQL are skipped
make test-all   # all tests; starts PostgreSQL in Podman on port 15433
```

The store, core and web tests need PostgreSQL. They make a new database
for each test and remove it at the end. Set `SQUARED_TEST_DATABASE_URL` to
use a different server. The user must be able to make databases.

The renderer tests compare the output with golden files. After an intended
change of the output, write new golden files and read the difference:

```bash
go test ./internal/render -update
git diff internal/render/testdata
```

## Development environment in Kind

```bash
make kind-up      # cluster, Argo CD, Forgejo, Harbor, Keycloak, PostgreSQL, RabbitMQ, Squared
make kind-deploy  # after a code change: build, load and restart Squared
make e2e          # end-to-end test
make kind-down    # remove the cluster
```

`make kind-up` takes about five minutes the first time. It is safe to run it
again. At the end it shows the addresses and the passwords. The passwords
are also in `.dev/` (Git ignores this directory).

`make kind-down` also removes the volume of the Kind node, which has the
images and the data of the cluster (more than 4 GB). With rootless
Podman, `kind delete` reports a network error and leaves this volume.
Older versions of the script left one volume for each cluster: find them
with `podman volume ls --filter dangling=true`.

The Forgejo pod has a sidecar with the Squared image (`squared
forgejo-config`, for the guard). `make kind-deploy` does not restart
Forgejo. After a change of that role, run
`kubectl -n forgejo rollout restart deployment/forgejo`.

| Service | Address | Sign-in |
|---|---|---|
| Squared | http://localhost:30080 | Keycloak |
| Argo CD | http://localhost:30081 | Keycloak, or the local `admin` account |
| Harbor | http://localhost:30002 | Keycloak, or the local `admin` account |
| Forgejo | http://forgejo.localhost:30300 | Keycloak. The local `forgejo-admin` account works only with the API: the web form accepts no passwords. |
| Keycloak | http://keycloak.localhost:30180 | `admin` (administration console, realm `master`) |

Sign in with the Keycloak user `platform-admin` (an administrator in all
four services) or `dev` (no administrator rights). One Keycloak session
signs you in to all services: after the first sign-in, the other services
do not ask for the password again. The local accounts are for emergencies
and for the scripts.

### Addresses

Two services have a `*.localhost` name. Browsers and curl resolve all
`*.localhost` names to the loopback address without any setup. Inside the
cluster, CoreDNS rewrites `keycloak.localhost` and `forgejo.localhost` to
the services, and the services have the same ports as their NodePorts.
Thus each of these addresses is the same inside and outside the cluster.
This is necessary:

- The issuer of the Keycloak tokens must be the same for the browser and
  for the services that check the tokens.
- The redirect address of Forgejo after the Keycloak sign-in comes from
  `ROOT_URL`, thus the browser must be able to open `ROOT_URL`.
- Argo CD compares the repository address in the push webhook of Forgejo
  (from `ROOT_URL`) with the address of its applications.

Git in the Argo CD repository server uses curl. Curl connects to
127.0.0.1 for all `*.localhost` names and does not ask DNS. A small
forwarder sidecar (`socat`) in the repository server thus listens on
127.0.0.1:30300 and forwards to the Forgejo service. All other clients are
Go programs, which ask DNS.

Forgejo uses its own PostgreSQL database in the namespace `forgejo`
(`deploy/kind/forgejo-db.yaml`). The guard reads the authentication sources
from a view in this database (see [security.md](security.md)). It reads
the Harbor configuration from a view in the database of the Harbor chart
(`registry` in the StatefulSet `harbor-database`).

The Kind node pulls images from Harbor at `localhost:30002` over plain
HTTP. The script writes a containerd `hosts.toml` file in the node for
this. Kube-proxy runs in iptables mode, because only that mode answers on
NodePorts at the localhost address of the node.

### Keycloak

Keycloak runs in development mode in the namespace `keycloak`. At each
start it imports the realm `squared` from
[`deploy/kind/keycloak/realm-squared.json`](../deploy/kind/keycloak/realm-squared.json)
if the realm does not exist. The realm has:

| Object | Purpose |
|---|---|
| Client `squared` | Squared. Confidential, authorization code flow with PKCE. Access tokens get the audience `squared-api`. |
| Client `argocd` | Argo CD. Confidential, with PKCE. |
| Client `harbor` | Harbor. Confidential. |
| Client `forgejo` | Forgejo. Confidential. |
| Client `squared-cli` | Development only: the password grant for API tokens in the end-to-end test. |
| Client `squared-guard` | Service account of the guard. It has `manage-realm` and keeps the permission `protected-groups` up to date. It reads the client mappers with `view-clients` and the identity providers with `view-identity-providers`. |
| Client `squared-worker` | Service account of the worker. It manages the groups `tenant-<tenant>-<role>`. `hack/kind-up.sh` limits it with fine-grained admin permissions. |
| Role `squared-protected-group-admin` | Nobody has it. Only its holders could manage the protected groups. |
| User profile | Only administrators can change the attribute `email` (`setup_keycloak_email` in `hack/kind-up.sh`), because Squared links accounts by the address and the worker finds members by it. |
| Group `squared-admins` | Administrators of Squared, Argo CD, Harbor and Forgejo. |
| User `platform-admin` (`platform-admin@squared.test`) | Member of `squared-admins`. Password in `.dev/keycloak-admin-user-password`. |
| User `dev` (`dev@squared.test`) | A normal user. Password in `.dev/keycloak-dev-user-password`. |

All clients have a `groups` claim with the full group paths, for example
`/squared-admins`. The realm file
has placeholders for the client secrets and the passwords. Keycloak takes
the values from the secret `keycloak`, which `hack/kind-up.sh` makes. The
users have fixed IDs, thus a new import after a restart gives the same
identities.

The administrator user is not called `admin`, because Forgejo reserves
that name and Harbor has a local `admin` account.

How each service uses Keycloak:

| Service | Configuration | Administrator rights | Other users |
|---|---|---|---|
| Squared | `SQUARED_OIDC_*` in the Kind overlay | `SQUARED_OIDC_ADMIN_GROUP` | Tenant roles in Squared. |
| Argo CD | `oidc.config` and RBAC in `deploy/kind/argocd-values.yaml` | `g, squared-admins, role:admin` | No rights. Tenants use Squared. |
| Harbor | `setup_harbor` in `hack/kind-up.sh` (Harbor API) | `oidc_admin_group` | Account at first sign-in, no project membership. |
| Forgejo | `gitea.oauth` in `deploy/kind/forgejo-values.yaml` | `adminGroup` | Account at first sign-in, no repository access. |

To add a user, open the administration console, select the realm
`squared`, and add the user with a verified email address.

### Push an image to the development Harbor

```bash
podman build -t localhost/hello:1.0.0 examples/hello
cat .dev/harbor-admin-password | podman login --tls-verify=false -u admin --password-stdin localhost:30002
podman push --tls-verify=false localhost/hello:1.0.0 localhost:30002/<tenant>/hello:1.0.0
```

The Harbor project of a tenant exists after the tenant status is `ready`.

### What the end-to-end test does

The test uses curl for most requests. For the sign-in through Keycloak it
uses the small program [`hack/ssoclient`](../hack/ssoclient/main.go),
which acts like a browser. Curl cannot do this step: Keycloak sets Secure
cookies, and curl accepts them over plain HTTP only for `localhost`, not
for `keycloak.localhost`. Browsers and the Go cookie jar accept them for
all `*.localhost` names.

`hack/e2e.sh`:

1. Signs in through the Keycloak login form and makes an API token on the
   account page.
2. Gets Keycloak access tokens for `platform-admin` and `dev`, and checks
   their rights in the API.
3. Checks the limits of the Keycloak account of the worker, also that a
   subgroup named `squared-admins` gives no rights.
4. Checks that the guard sees `/squared-admins` in the configurations of
   all four services, that a new group in the Argo CD RBAC is made and
   protected, and that it is released when the line goes. It does the same
   with a `g` line in the policies of an AppProject role, and checks that an
   unknown claim in the Argo CD `scopes` gives an error, that an Argo CD
   credential template that ends in the host name gives an error, that a
   cluster without TLS checks gives a warning, that a tenant project for
   all namespaces, an application without automatic sync, an application
   of the project `default` with the platform repository, a root
   application without `selfHeal`, and `application.namespaces` with an
   application in the namespace `default`, an ApplicationSet that reads
   the platform repository, SCM generators without limits, and an
   ApplicationSet that takes over an application of Squared, a template
   patch with the finalizer of Argo CD, a generator template with a tenant
   project, a plugin generator with an unsafe token, a cluster decision
   resource generator for Argo CD applications, read rights of the
   ApplicationSet controller in another namespace, read rights of the
   application controller on Secrets, the right of the API server to run
   commands in pods, a token of the API for the repository server and for
   Redis, the Role of the job for the password of Redis, more rights for
   the account of Dex, and the
   ApplicationSet policy `sync` give errors,
   and that Keycloak
   mappers that write `groups` on the client `argocd` and `email` from
   another attribute on the client `forgejo` give errors. It also turns on
   duplicate email addresses and lets users edit their address in the
   realm, and checks the errors. It does the same
   with a change of the Forgejo administrator group through the Forgejo
   command line, which writes the same table as the web interface. Then it
   renames the field `AdminGroup` in the database, as an upgrade could, and
   checks that the guard reports it and keeps the protection. It writes
   `ACCOUNT_LINKING = auto`, `ALLOW_ONLY_EXTERNAL_REGISTRATION = false`,
   `ENABLE_INTERNAL_SIGNIN = true` and
   `ALLOWED_HOST_LIST = *.svc.cluster.local` into `app.ini` of the running
   Forgejo pod, adds `ALLOW_LOCALNETWORKS = true` in `[migrations]` and
   `IMPORT_LOCAL_PATHS = true` in `[security]` and `SKIP_TLS_VERIFY = true`
   in `[webhook]`, and checks that the guard reports them. It adds a deploy
   key with write access to `squared/platform` and checks the error. It
   gives `argocd-reader` write access as a collaborator, and then through a
   new team, and checks the errors. It checks that the local Forgejo
   administrator cannot write to `squared/platform`, and that the guard
   reports a branch rule that does not apply to administrators, a webhook
   of `squared/platform` to an unexpected address, and a push mirror of
   `squared/platform`. After the
   tenant is made, it checks that the tenant repository has the rule, and
   that the last delivery of the webhook to Argo CD succeeded. It does the
   same for Harbor: it changes the administrator group through the Harbor
   API, then renames the key `oidc_admin_group` in the database.
5. Signs in to Argo CD, Harbor and Forgejo through Keycloak as both users,
   and checks the administrator rights in each service. Then it signs in
   to Forgejo as two new Keycloak users with the address
   `dev@e2e-intruder.test` and with the name and address of the local
   Forgejo administrator, and checks that neither gets an existing
   account. It checks that the Forgejo sign-in page has no password form
   and that `dev` cannot set a Forgejo password. It makes a Forgejo token
   for `dev`, disables `dev` in Keycloak, then makes `dev` a Forgejo
   administrator, and checks that the guard reports both. It does the
   same with an SSH key of `dev` and a disabled `dev`.
6. Makes a tenant, and checks that Argo CD repairs a change by hand of
   its project. Adds `dev` as developer, and waits until `dev` has the
   Harbor project and can read the Forgejo repository.
7. Pushes the example image as `dev` with the Harbor CLI secret.
8. Deploys the image and checks the answer of the service. Checks that
   the guard has no errors with the application and the project of the
   tenant.
9. Checks that `dev` can read, but not sync, the application in Argo CD,
   and sees its pods.
   As administrator, it uses the web interface of Argo CD: it shows the
   Deployment, its events and the logs of its pod, and runs the action
   `restart`.
10. Checks that a pod in another namespace cannot connect to the service.
11. Changes a value (revision 2) and checks the new answer.
12. Rolls back to revision 1 (revision 3) and checks the old answer.
13. Removes `dev` from the tenant and checks that `dev` loses the access.
14. Removes the deployment and checks that the objects are gone.
15. Removes the tenant and checks that its Argo CD project is gone.

Set `KEEP_TENANT=1` to keep the tenant after the test, so that you can
examine it.

## Run outside the cluster

You can run the server on your computer against PostgreSQL and RabbitMQ in
Podman:

```bash
podman run -d --name squared-dev-db -e POSTGRES_USER=squared -e POSTGRES_PASSWORD=dev \
  -e POSTGRES_DB=squared -p 127.0.0.1:15432:5432 docker.io/library/postgres:17-alpine
podman run -d --name squared-dev-mq -p 127.0.0.1:15673:5672 docker.io/library/rabbitmq:4.1-alpine
echo 'admin password 1' > /tmp/admin-password
SQUARED_DATABASE_URL=postgres://squared:dev@127.0.0.1:15432/squared \
SQUARED_AMQP_URL=amqp://guest:guest@127.0.0.1:15673/ \
SQUARED_CATALOG_DIR=catalog SQUARED_REGISTRY_HOST=localhost:30002 \
SQUARED_LISTEN=127.0.0.1:18080 SQUARED_LOG_FORMAT=text \
SQUARED_BOOTSTRAP_ADMIN_EMAIL=admin@example.com \
SQUARED_BOOTSTRAP_ADMIN_PASSWORD_FILE=/tmp/admin-password \
go run ./cmd/squared serve
```

The worker needs Forgejo, Harbor and Kubernetes. Use the Kind environment
for it.

## Coding rules

- Write Go in the style of the code around it. Run `gofmt`.
- Use the standard library. Add an external module only for complex
  protocols or security functions, and record the reason in
  [architecture.md](architecture.md) (decision D9).
- Wrap errors with context: `fmt.Errorf("make project %s: %w", name, err)`.
- Put business rules and permission checks in `internal/core`, not in the
  web or API handlers.
- Task handlers must be idempotent.
- Write all documentation and comments in Simple Technical English: short
  sentences, active voice, simple tenses, and one word for one meaning.
