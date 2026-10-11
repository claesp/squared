# User guide

This guide tells you how to use the Squared web interface. The terms are
in the glossary of [architecture.md](architecture.md#glossary).

## Sign in

Open the address of Squared and select **Sign in with Keycloak**. Sign in
at Keycloak with your Keycloak account. Keycloak then sends you back to
Squared. At your first sign-in, Squared makes your account from your
Keycloak name and verified email address.

Keycloak manages your password, your name and your email address. Change
them in Keycloak. **Sign out** in Squared also signs you out of Keycloak.

If your organization does not use Keycloak, sign in with your email
address and password. An administrator gives you your first password.
Change it on the **Account** page.

## Navigation

The column at the left has all pages. **Platform** has the list of your
tenants and the catalog of packages. When you open a tenant, a group for
the tenant shows its pages: **Overview**, **Members**, **Audit log** and,
for platform administrators, **Settings**. Platform administrators also
see **Administration**: users, a new tenant and the audit log of the
platform. **You** has your account. On a narrow screen, the column is a
row of links at the top of the page.

## Roles

Members of the Keycloak group `squared-admins` are platform
administrators. They can also sign in to Argo CD, Harbor and Forgejo with
the same Keycloak account, as administrators. A platform administrator adds tenants. A tenant owner can
add only users that exist in Squared: ask the user to sign in one time
first, or ask an administrator to add the user. In a tenant, each member
has one role:

| Operation | viewer | developer | owner |
|---|:---:|:---:|:---:|
| Read deployments, environments, members and the audit log | yes | yes | yes |
| Make, change, roll back and remove deployments | | yes | yes |
| Add environments and remove empty environments | | | yes |
| Add members, change roles and remove members | | | yes |
| Prepare the tenant again after a failure | | | yes |

Only a platform administrator can change the name and the quota of a
tenant, and remove a tenant.

## Remove a tenant

A platform administrator removes a tenant on the **Settings** page of the
tenant. First remove all deployments of the tenant. Squared then removes
the Argo CD project, the quotas and the access rules, archives the Git
repository and removes the tenant and its members from the database. The
audit events stay. The namespaces and the Harbor project with the images
stay, until an administrator removes them by hand. See
[operations.md](operations.md#remove-an-environment-namespace).

## Your access in Harbor, Forgejo and Argo CD

Your role in a tenant also gives you access in the platform tools. Sign
in to each tool with Keycloak:

| Your role | Harbor project of the tenant | Git repository of the tenant (Forgejo) | Argo CD |
|---|---|---|---|
| owner | push, pull and delete images | read | read applications and pod logs |
| developer | push and pull images | read | read applications and pod logs |
| viewer | pull images | read | read applications and pod logs |

Squared manages this access. When an owner changes your role or removes
you, the change applies at your next sign-in to Harbor or Argo CD, and
immediately in Forgejo.

## Push an image

Each tenant has a project in the Harbor registry. The tenant page shows
the address, for example `harbor.example.com/acme`. Owners and developers
can push images.

1. Sign in to the Harbor web interface with Keycloak. Open your user
   profile and copy the **CLI secret**. Then sign in with your container
   tool: `podman login harbor.example.com` with your Harbor user name and
   the CLI secret as password. A CI system can also use a Harbor robot
   account.
2. Tag the image: `podman tag shop:1.4.2 harbor.example.com/acme/shop:1.4.2`
3. Push the image: `podman push harbor.example.com/acme/shop:1.4.2`

In Squared, give the image as `shop:1.4.2`. Do not give the registry or
the project. Always give a tag or a digest. Squared does not accept an
image without a tag, because "latest" can change.

## Make a deployment

1. Open the tenant and select **New deployment**.
2. Select a package. The [catalog](packages.md) page describes all
   packages.
3. Give a deployment name. You cannot change it later.
4. Select the environment.
5. Give the parameter values. The form shows a description and the
   default value of each parameter.
6. Optionally, give a note about the change.
7. Select **Make deployment**.

Squared shows the deployment page. The page loads again every five seconds
until the work is complete. The status changes from `pending` to
`committed` to `deployed`. See the
[status values](architecture.md#status-values).

## Change a deployment

1. Open the deployment and select **Change**.
2. To use a newer package version, select it at the top of the form and
   select **Use this version**. The form keeps your values.
3. Change the values and select **Save and deploy**.

Each change makes a new revision. The revision table on the deployment page
shows all revisions with the user, the time, the note and the Git commit.

Squared resolves the image tag to a digest when it processes a revision.
If you push a new image with the same tag, the running deployment does not
change. Make a new revision to use the new image: open **Change** and
select **Save and deploy**.

## Roll back

On the deployment page, select **Roll back to this** in the row of an
earlier revision. Squared makes a new revision that is a copy of the
earlier revision: the same package version, the same values and the same
image digests.

## Remove a deployment

1. Open the deployment.
2. Open **Remove the deployment** and type the deployment name.
3. Select **Remove deployment**.

Argo CD removes all objects of the deployment from the cluster. Then
Squared removes the deployment and its revisions. The Git history stays.

## Environments

An owner can add an environment on the tenant page. Squared makes the
namespace `<tenant>-<environment>` with the quota of the tenant. To remove
an environment, first remove all its deployments. An administrator removes
the empty namespace from the cluster later, because Squared never removes
namespaces automatically.

## When something fails

- The deployment page shows the error of the last attempt in the revision
  table.
- A `failed` status means that the worker stopped. Repair the cause, for
  example push the missing image. Then select **Process again**.
- A `degraded` status means that Argo CD applied the manifests, but the
  pods are not healthy. Check the health check path and the logs of your
  program.
- The **Audit log** page of the tenant shows all changes of the tenant.

## API tokens

Scripts and CI systems use the [API](api.md). Make a token on the
**Account** page. The token shows only one time. Copy it into the secret
store of your CI system. Remove tokens that you do not use.
