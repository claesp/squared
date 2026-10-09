# Squared

Squared is a management platform for a cloud service that runs on
Kubernetes. Tenants use a web interface (or a JSON API) to deploy
applications from a catalog of tested packages. Squared does not apply
changes to the cluster itself. It writes the desired state to Git
(Forgejo), and Argo CD applies it. Images come from Harbor.

The documentation is in the [docs](docs/) folder.

## Status

Work in progress.
