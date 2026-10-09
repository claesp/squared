# Security

This document lists the threats that Squared protects against and the
controls that it uses. Read [architecture.md](architecture.md) first.

## Trust boundaries

| Boundary | Who is on the other side | Control |
|---|---|---|
| Browser to server | Users, attackers on the network | TLS (at the ingress), session cookies, CSRF tokens, security headers |
| API client to server | Scripts and CI systems | Personal access tokens |
| Server to worker | Only the server writes tasks | RabbitMQ credentials; tasks carry only IDs, the worker reads all data from the database |
| Worker to Forgejo, Harbor, Kubernetes | External systems | One credential for each system, only in the worker |
| Tenant to tenant | Other tenants in the same cluster | Namespaces, Argo CD projects, network policies, quotas, Pod Security Admission |

## Authentication

- **Passwords** are hashed with Argon2id (19 MiB memory, 2 iterations, 1
  thread, 16 byte salt). This follows the OWASP recommendation. A password
  has at least 12 characters.
- **Sign-in** gives the same answer and uses the same time when the user
  does not exist. This prevents user enumeration.
- **Rate limit.** The server accepts 10 sign-in attempts in 5 minutes for
  each client address and for each email address.
- **Sessions** use a random token with 160 bits of entropy. The database
  keeps only the SHA-256 hash of the token. A copy of the database thus
  does not give access. Sessions expire after 12 hours
  (`SQUARED_SESSION_TTL`).
- **Session cookies** have the `HttpOnly`, `Secure` and `SameSite=Lax`
  attributes and the `__Host-` prefix.
- **A password change** ends all sessions of the user. **Disabling a user**
  ends all sessions of the user and blocks the API tokens of the user.
- **API tokens** have the prefix `sqp_` and 160 bits of entropy. The
  database keeps only the SHA-256 hash. A token can expire. The token
  shows only one time, when the user makes it.

## Authorization

- Platform administrators can do all operations.
- In a tenant, a member has one role: `owner`, `developer` or `viewer`.
  See the [user guide](user-guide.md) for the permissions of each role.
- `core.Service` checks the permission in each operation. The web interface
  and the API have no separate permission logic.
- A user who is not a member of a tenant gets "not found", not "forbidden".
  Thus users cannot find out which tenants exist.
- A tenant always keeps at least one owner.

## Web interface

- **No JavaScript.** The Content Security Policy is
  `default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`.
  An injected script cannot run.
- **Output encoding.** `html/template` escapes all values for their
  context.
- **CSRF.** Each form has a token from the session. The server also uses
  `http.CrossOriginProtection`, which rejects state changes from other
  sites (`Sec-Fetch-Site` and `Origin` headers).
- **Open redirects.** The `next` parameter of the sign-in page accepts only
  paths on the same site.
- **Other headers.** `X-Frame-Options: DENY`, `X-Content-Type-Options:
  nosniff`, `Referrer-Policy: same-origin`, `Cross-Origin-Opener-Policy`,
  `Permissions-Policy` and optional HSTS (`SQUARED_HSTS=true`).
- **Limits.** Request bodies have a limit of 1 MiB. The HTTP server has
  read, write and idle timeouts.

## Tenant isolation in the cluster

Each tenant environment gets these objects from the platform repository:

| Object | Effect |
|---|---|
| Namespace with Pod Security Admission `restricted` | Kubernetes rejects pods that run as root, have privileges, use host namespaces or add capabilities. |
| ResourceQuota | Limits CPU, memory and pods. Blocks LoadBalancer and NodePort services and persistent volume claims. |
| LimitRange | Gives default resources to containers without resources. |
| NetworkPolicy | Accepts connections only from the same namespace and from the ingress controller. |
| Argo CD AppProject | Accepts manifests only from the tenant repository, only for the tenant namespaces, and only these kinds: ServiceAccount, Service, ConfigMap, Deployment, CronJob, Ingress, PodDisruptionBudget. No cluster objects. |

The renderer also gives each workload secure settings that tenants cannot
change: no service account token, read-only root file system, no privilege
escalation, all capabilities dropped, `RuntimeDefault` seccomp profile, a
memory limit and a writable `/tmp` with a size limit.

## Supply chain

- Images must come from the Harbor project of the tenant. The renderer
  rejects an image setting that does not come from an image parameter.
- The worker resolves each tag to a digest. Pods use the digest.
- The base images of Squared are pinned by digest.
- The Helm charts of the development environment are pinned by version.
- Squared has three external Go modules (see D9 in the architecture).
  `go.sum` pins their content.

## Secrets

- Squared reads secrets from files (`*_FILE` variables). Kubernetes mounts
  them from Secret objects with mode 0440.
- The server gets only the database URL, the broker URL and the first
  administrator password. Only the worker gets the Forgejo token and the
  Harbor password.
- Git never contains secrets. Parameter values are in Git, thus the forms
  tell users not to put secrets in environment variables.

## Audit

Squared records each change in the `audit_events` table: the user, the
action, the object and details. Owners and members see the events of their
tenant. Administrators see all events. Git keeps a second record: each
commit has the revision and the user.

## Production checklist

1. Put TLS in front of the server (ingress or load balancer). Set
   `SQUARED_HSTS=true` and `SQUARED_TRUST_PROXY=true` if a proxy sets
   `X-Forwarded-For`.
2. Use TLS for PostgreSQL (`sslmode=verify-full`) and RabbitMQ (`amqps://`).
3. Give the worker a Forgejo account that is the owner of the Squared
   organization only. Give Argo CD a separate read-only account.
4. Give the worker a Harbor robot account with system permissions for
   projects and robots, not the `admin` account.
5. Set a webhook secret in Forgejo and in Argo CD.
6. Restrict access to the Argo CD, Forgejo and Harbor web interfaces.
7. Make backups of PostgreSQL and Forgejo. Git and the database together
   are the complete state.
8. Remove the first administrator password secret after the first start,
   or change the password in the web interface.
