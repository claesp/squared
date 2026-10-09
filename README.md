# Squared

Squared is a management platform for a cloud service on Kubernetes. Tenants
use a web interface or a JSON API to deploy applications from a catalog of
tested packages.

Squared does not apply workloads to the cluster itself. It writes the
desired state to Git in [Forgejo](https://forgejo.org), and
[Argo CD](https://argo-cd.readthedocs.io) applies it. Images come from
[Harbor](https://goharbor.io). Tasks go from the web server to the worker
through [RabbitMQ](https://www.rabbitmq.com).

## Features

- **Multi-tenancy.** Each tenant gets its own namespaces, Git repository,
  Harbor project and Argo CD project, with quotas, network isolation and the
  "restricted" Pod Security Standard.
- **Users and roles.** Platform administrators, and owners, developers and
  viewers in each tenant. Argon2id passwords, sessions with CSRF protection,
  and personal API tokens.
- **Component-based packages.** Administrators define packages from
  components (`web`, `worker`, `cron`) with typed parameters. The renderer
  adds secure settings that tenants cannot change.
- **Reproducible revisions.** Each change is a revision with a copy of the
  package, the values and the image digests. A rollback gives the same
  result as before. Each revision maps to one Git commit.
- **Web interface without JavaScript**, and a JSON API with the same rules.
- **Reliable task processing**: transactional outbox, publisher confirms,
  idempotent tasks, retries with delays and a dead queue.
- **Audit log** of all changes, in the database and in Git.
- **Small dependency set**: the Go standard library, plus clients for
  PostgreSQL and RabbitMQ, and `x/crypto` for Argon2id.

## Quick start

You need Go, Podman, Kind, kubectl, Helm, curl, jq and openssl. See
[docs/development.md](docs/development.md) for details.

```bash
make kind-up   # makes the complete environment in Kind (about 5 minutes)
make e2e       # deploys an example application from end to end
```

Then open http://localhost:30080 and sign in as `admin@squared.test`. The
password is in `.dev/squared-admin-password`.

## Build and test

```bash
make check     # formatting, vet, catalog check
make test-all  # all tests (starts PostgreSQL in Podman)
make image     # container image
```

## Documentation

The documentation is in [docs/](docs/README.md):

- [User guide](docs/user-guide.md)
- [API](docs/api.md)
- [Packages](docs/packages.md)
- [Architecture](docs/architecture.md)
- [Security](docs/security.md)
- [Configuration](docs/configuration.md)
- [Operations](docs/operations.md)
- [Development](docs/development.md)
