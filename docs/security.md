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

### Sign-in with Keycloak (OpenID Connect)

- Squared uses the authorization code flow with PKCE (`S256`), a random
  state and a nonce. The client is confidential: the token request has the
  client secret.
- The state protects against forged callbacks. The server keeps each
  sign-in attempt in the database for ten minutes and uses it one time.
  A cookie binds the attempt to the browser; a callback without the cookie
  fails.
- The server verifies the signature, the issuer, the audience, the expiry
  time and the nonce of the ID token.
- Squared links an identity to an existing user only if the email address
  is verified, and only if the user has no other identity.
- The administrator flag comes from a Keycloak group. Removing a user from
  the group removes the administrator rights at the next sign-in.
- Sign-out also ends the Keycloak session.
- Passwords, second factors, brute force protection and password policies
  are in Keycloak. In the development realm, brute force protection is on
  and passwords have at least 12 characters.
- With OIDC, the password sign-in of Squared is off by default
  (`SQUARED_LOCAL_LOGIN`).
- Access tokens from Keycloak are valid for the API only with the audience
  `squared-api`. They expire after five minutes in the development realm.
  A disabled user in Squared cannot use them.
- A Squared session lives until it expires (`SQUARED_SESSION_TTL`), also if
  the Keycloak session ends earlier. Use a short session time if this is a
  problem. Disable the user in Squared to end all sessions at once.

### Sign-in to Argo CD, Harbor and Forgejo

- All three services use the Keycloak realm, each with a confidential
  client. Argo CD also uses PKCE.
- Only members of `squared-admins` get administrator rights. Argo CD gives
  other users no rights (`policy.default` is empty). Harbor and Forgejo
  make an account for other users, but give no access to projects or
  repositories.
- Forgejo makes accounts only from Keycloak sign-ins
  (`ALLOW_ONLY_EXTERNAL_REGISTRATION`). It does not link a Keycloak
  identity to an existing local account.
- The local administrator accounts (`admin` in Argo CD and Harbor,
  `forgejo-admin` in Forgejo) stay for emergencies and automation. Keep
  their passwords in a secret store, and use them only when Keycloak is not
  available.

### Access of tenant members to the platform tools

- Squared controls the access. The worker removes group members, Harbor
  members and team members that somebody added by hand.
- A disabled Squared user loses the tenant groups in Keycloak and the
  Forgejo teams. Disable the user in Keycloak too, to stop all sign-ins.
- Tenant members get read access only to Git and Argo CD. Only Harbor gives
  them write access (push), to their own project.
- **Rights of the worker in Keycloak.** Fine-grained admin permissions
  (version 2) limit the service account `squared-worker`. It has no admin
  role other than `query-users` and `query-groups`, which only permit
  searches. Its permissions:

  | Resources | Scopes | Effect |
  |---|---|---|
  | All users | `view`, `manage-group-membership` | Find users and change their group memberships. |
  | All groups | `view`, `view-members`, `manage`, `manage-membership` | Make and remove groups, and change their members. The same permission lets the guard make groups. |
  | Protected groups (`squared-admins`) | `view`, `manage`, `manage-membership`, only for holders of the realm role `squared-protected-group-admin` | A permission for a specific group replaces the permission for all groups. Nobody has the role, thus the worker can neither see nor change these groups, nor add or remove their members. |

  The worker can read users, and thus also the members of the protected
  groups. It cannot change them.

  To add a user to a group, Keycloak needs both `manage-group-membership`
  on the user and `manage-membership` on the group. The worker thus cannot
  add anybody to a protected group. It cannot make, change, disable or
  remove users, reset passwords, assign roles, impersonate users, read
  clients or keys, or change the realm. The end-to-end test checks these
  limits. The protected permission does not have the member scopes
  (`view-members`, `manage-membership-of-members` and so on), because these
  apply to the members themselves: the worker must be able to put an
  administrator into a tenant group.
- **Automatic protection.** Keycloak cannot limit the permission to the
  groups that the worker made, thus the protection is a list of groups.
  The guard (decision D14 in the architecture) keeps the list up to date
  from the configurations of the services. A group that a configuration
  gives rights to is protected within one run (30 seconds), and the guard
  makes a missing group before anybody else can.
- **Rights of the guard.** The guard has the realm-management roles
  `manage-realm` (no smaller role can change fine-grained admin
  permissions), `view-users`, `query-groups`, `view-clients` (to read
  the client mappers and to find the `admin-permissions` client) and
  `view-identity-providers` (to read the email trust of identity
  providers), and the
  group permission of the worker (to make groups). Keycloak removes the
  client secrets from the answers unless the account can manage the
  client, and only an account that can manage a client can read its
  secret: `view-clients` gives no secrets. It also removes the secrets of
  identity providers and user federation from the answers. It has no `manage-group-membership` on
  users, thus it cannot change memberships. This is a strong account. The guard thus runs
  alone in the namespace `squared-guard`, accepts no connections, reads
  the configurations with read-only Kubernetes roles (named objects where
  Kubernetes permits it), a Harbor robot account that can only list
  projects and members, and an Argo CD account that can only read the
  repositories and the clusters. Its code writes only the resources of one
  permission and makes groups. Protect its secret like a Keycloak
  administrator password.
- **Limits of the guard.** It sees only the sources in its configuration.
  The guard makes only top-level groups.
- **Forgejo authentication sources.** Forgejo has no API for them, and a
  change in the web interface is only in the table `login_source`. That
  table also has the OAuth client secrets. Thus the guard gets a database
  role, `squared_guard`, that can read only the view
  `squared_guard_auth_sources`
  ([`deploy/kind/forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql)).
  The view has the name, the type, the active flag and the field names
  (never the values) of each source, and the group fields of the OAuth
  sources: administrator, restricted, team map, required claim and quota
  map. A test checks that the role cannot read the table and that the view
  has no secret.
- **Renamed Forgejo fields.** The view reads the group fields by name. If
  a Forgejo upgrade renamed a field, the view would give an empty value and
  the guard would release the group without notice. Thus the guard
  compares the field names of each OAuth source with the fields of
  Forgejo 15 that it knows (Forgejo writes all fields, also empty ones):

  | Change | Result |
  |---|---|
  | A known group field is missing (renamed or removed) | Error |
  | A new field with a name like group, team, admin, restrict, claim, role, quota, permission, access or org | Error |
  | A source with the fields of an OAuth source but another type number | Error |
  | A new field with another name, or a missing field without group meaning | Warning, until an operator adds it to `ignoreFields` |

  An error stops the source: the guard keeps the current protection,
  adds the groups that it could read, and the run counts as failed. The
  end-to-end test renames `AdminGroup` in the database and checks this. The guard also
  protects the groups of inactive sources, because an administrator can
  turn them on. The network policy of the database accepts only Forgejo
  and the namespace of the guard. A group that becomes protected keeps the members that it had
  before; the guard warns about them.
- **Keycloak client mappers.** The protection of a group helps only if
  membership of the group is the only way to get its path into the
  `groups` claim. Other mappers can write that claim: a fixed value
  (`oidc-hardcoded-claim-mapper`), a user attribute, realm roles (the
  built-in scope `microprofile-jwt` does this), or even a value from the
  sign-in request (`oidc-claims-param-value-idtoken-mapper`). The same is
  true for the email address: Squared links a new sign-in to an existing
  account with the same address if `email_verified` is true, and the
  worker finds the Keycloak user of a tenant member by the address. A
  mapper that writes `email` from another attribute, which the user can
  perhaps change, gives the account of another person. The guard reads the mappers of the clients that the
  services use, with the mappers of their default and optional client
  scopes:

  | Change | Result |
  |---|---|
  | A mapper of another type writes a group claim, or a part of it (`groups.x`) | Error |
  | A group membership mapper without full paths: a subgroup with the same name gives the same rights | Error |
  | A mapper writes `email` (or a part of it), and it is not a user property or user attribute mapper for `email` | Error |
  | A mapper writes `email_verified`, and it is not a user property or user attribute mapper for `emailVerified` | Error |
  | A mapper type that Keycloak 26.8 does not have, for example a script mapper; it can write any claim | Error, until an operator adds the type to `ignoreFields` |
  | The guard does not see a client of its configuration, for example without `view-clients` | Error |
  | A client of the configuration has no allowed mapper for `groups`, `email` or `email_verified` | Warning |

  It also checks every other client with an audience mapper for one of
  these clients or for a custom audience of a service (`squared-api`),
  because the services accept its tokens. The built-in clients of the
  realm, for example `account`, may ask for `microprofile-jwt`, but their
  tokens have no audience of a service. The guard checks the mappers
  also when they are in no token: a later change of one flag would make
  them active. The end-to-end test adds a mapper that writes `groups` to
  the client `argocd`, and a mapper that writes `email` from the attribute
  `nickname` to the client `forgejo`, and checks the errors. Limit: the
  guard does not read the client roles that `oidc-audience-resolve-mapper`
  turns into audiences; give the service clients no client roles.
- **Keycloak realm email settings.** A correct `email` mapper helps only
  if a person can get only an address that the person owns, or that an
  administrator gave. Squared links a new sign-in to an existing user by
  a verified address, and the worker finds the Keycloak user of a tenant
  member by the address. Keycloak permits users to change their own
  address by default; `hack/kind-up.sh` permits it only to
  administrators. The guard reads the realm settings:

  | Setting | Result |
  |---|---|
  | `duplicateEmailsAllowed` is on | Error |
  | The attribute `email` of the user profile has `user` in `permissions.edit` | Error |
  | `registrationAllowed` is on and `verifyEmail` is off | Error |
  | `registrationEmailAsUsername` and `editUsernameAllowed` are on | Error |
  | An identity provider or user federation with `trustEmail` on | Error, until an operator adds its alias or name to `ignoreFields` |
  | Another identity provider or user federation | Warning, until an operator adds its alias or name to `ignoreFields` |

  The end-to-end test turns on `duplicateEmailsAllowed`, and lets users
  edit their address, and checks the errors.
- **Argo CD configuration.** Argo CD reads its RBAC only from the
  ConfigMap `argocd-rbac-cm` and from the AppProjects. The guard reads the
  same objects through the Kubernetes API, thus it sees all changes, also
  from the Argo CD web interface and CLI. It reads them as Argo CD 3.5
  does:

  - Argo CD adds the `policies` of a project role to its RBAC without a
    check. The API of Argo CD accepts only `p` lines for the role, but
    `kubectl` can write a `g` line, for example
    `g, /group, role:admin`. The guard protects the subjects of such lines.
  - A `p` line with a group as subject gives no rights alone: Argo CD
    checks only the groups that are in a `g` line.
  - Argo CD compares the subjects of `g` lines exactly with the values of
    the claims in the key `scopes`. `policy.matchMode` applies only to
    resources and objects, not to subjects.

  | Change | Result |
  |---|---|
  | A new key in `argocd-rbac-cm` (all its keys are about rights) | Error |
  | A claim in `scopes` other than `groups` and `email`: the guard cannot tell which Keycloak groups give a value of it | Error |
  | `scopes` without `groups` | Warning |
  | A new AppProject or role field with a name like group, team, admin, restrict, claim, role, quota, permission, access or org | Error |
  | A new AppProject or role field with another name | Warning, until an operator adds it to `ignoreFields` |

  Kubernetes removes fields that the CRD does not have, thus a new field
  comes only with an upgrade of the CRD. The end-to-end test adds a `g`
  line to a project role and adds an unknown claim to `scopes`.
- **Repository credentials of Argo CD.** Argo CD reads the GitOps
  repositories with credentials in Secrets of its namespace. The guard
  does not read these Secrets: Kubernetes cannot give access to their
  names without their values. It reads the repositories and the
  credential templates through the API of Argo CD, which removes the
  passwords, tokens and keys, with a token of the local account
  `squared-guard`. That account has only the rights `repositories, get` and
  `write-repositories, get`, and cannot sign in.

  | Repository or credential template | Result |
  |---|---|
  | A template whose address ends in the host name (for example `https://git.example`): Argo CD uses a template for all addresses that start with it, thus also for `https://git.example.evil/` | Error |
  | A repository or template of the GitOps repositories (`repoPrefixes`) that does not check the TLS certificate or the SSH host key (`insecure`) | Error |
  | Another repository or template without the check | Warning |
  | A write repository or write template of the GitOps repositories, while the source hydrator of Argo CD is on: Argo CD can push to them | Error |
  | No repository or template of the GitOps repositories is visible: Argo CD cannot read them, or the account of the guard has no right | Error |

  When the hydrator is off, Argo CD answers 501 for the write
  repositories, and nothing pushes. Limit: the API of Argo CD 3.5 shows
  only the write templates that have the address of a read template, thus
  the guard can miss a write template while the hydrator is on; it warns
  about this. The end-to-end test adds a template that ends in the host
  name and checks the error.
- **Cluster credentials of Argo CD.** Argo CD sends the token of a cluster
  with each request to its API server, and applies the manifests of the
  tenants there. The guard reads the clusters through the API of Argo CD,
  which removes the tokens, the keys and the settings of exec providers,
  with the same account (right `clusters, get`).

  | Cluster | Result |
  |---|---|
  | A cluster to which Squared deploys (`clusterServers`) without the check of the TLS certificate (`insecure`) or with an `http://` address: a person in the network can take its token or change the workloads | Error |
  | Another cluster like that | Warning |
  | A cluster Secret for `https://kubernetes.default.svc`: Argo CD then uses its credentials instead of its own service account | Warning: check the rights of these credentials |
  | The cluster of Squared is not visible: Argo CD has no such cluster, or the account of the guard has no right | Error |

  Limit: the API never shows exec providers, which run a command in the
  pods of Argo CD; only a person who can make cluster Secrets can add one.
  The end-to-end test adds a cluster Secret for another cluster without
  the check and checks the warning.
- **Tenant projects of Argo CD.** The AppProject of a tenant is the
  second line of the tenant isolation (see
  [Tenant isolation in the cluster](#tenant-isolation-in-the-cluster)):
  Argo CD refuses an object outside it, also when a manifest asks for it.
  The guard reads all AppProjects through the Kubernetes API (right `list`
  on `appprojects`) and compares each project with a name that starts with
  `tenant-` with what Squared puts into it.

  | Tenant project | Result |
  |---|---|
  | A destination with a cluster that is not in `clusterServers`, or a cluster by its name instead of its address | Error |
  | A destination with a namespace that is not `<tenant>-<environment>`, for example `*` or `kube-system` | Error |
  | A source repository that is not the repository of the tenant | Error |
  | Cluster kinds, no list of namespaced kinds (Argo CD then permits all of them), or namespaced kinds that the renderer does not make | Error |
  | `sourceNamespaces` or `destinationServiceAccounts` | Error |
  | A role other than `member`, tokens of a role, or a policy other than the read access of `member` | Error |
  | A project with the name `tenant-<slug>` that Squared did not make (no label `app.kubernetes.io/managed-by: squared`) | Error |

- **Applications of Argo CD.** An application tells Argo CD what to
  deploy, from where, and if it changes back changes by hand. The guard
  reads the applications in all namespaces through the Kubernetes API
  (right `list` on `applications` in the cluster).

  | Application | Result |
  |---|---|
  | An application of Squared (labels `app.kubernetes.io/managed-by: squared` and `squared.dev/managed: "true"`) without automatic sync, or with `prune` or `selfHeal` off: Git and the cluster can differ | Error |
  | An application of Squared with another project than `tenant-<slug>`, another repository than the tenant repository, or a branch or tag instead of a commit | Error |
  | An application of Squared with another source path than the directory of its deployment (`<environment>/<deployment>`): the directory of another deployment or environment has other manifests | Error |
  | An application of Squared with more than one source, a source hydrator, or a source field other than `repoURL`, `targetRevision` and `path` (for example `helm`, `kustomize`, `plugin`, `chart` or `directory`, which has Jsonnet): Argo CD then changes the manifests before it applies them | Error |
  | An application of Squared with a destination that is not in `clusterServers` or not the namespace of its environment, or with labels for the environment and the deployment that do not agree with its name | Error |
  | An application of Squared with other sync options than `CreateNamespace=false` and `PruneLast=true`, or with `ignoreDifferences` | Error |
  | An application of Squared in another namespace than the namespace of Argo CD | Error |
  | An application of Squared, or the root application, with an owner (`ownerReferences`), for example an ApplicationSet that took it over | Error |
  | An application that Squared did not make in a tenant project | Error |
  | An application that Squared did not make with a source in the GitOps repositories (`repoPrefixes`), also as one of more sources or for the source hydrator: it applies the manifests of a tenant without the limits of the tenant project, or the platform objects | Error |
  | The root application (`name`) without automatic sync, `prune` or `selfHeal`: a change by hand of a tenant project stays | Error |
  | The root application with another source than `rootSource`, more than one source, a source hydrator, or a source field other than `directory.recurse`: for example `directory.exclude` can hide the tenant projects from it | Error |
  | The root application is not visible | Error |

  Argo CD changes a tenant project back after a change by hand only while
  the root application heals itself, thus the two checks work together.
  The guard compares the addresses of the repositories as Argo CD does:
  without the user, without `.git` and `/` at the end, and without
  differences in case. Limit: the guard knows only the addresses in
  `repoPrefixes`. Put all addresses at which Argo CD can reach Forgejo
  into it, for example also the address of the Service in the cluster.
  The guard names an application in another namespace as
  `<namespace>/<name>`.
  The end-to-end test adds a tenant project for all namespaces, an
  application without automatic sync, and an application of the project
  `default` with the platform repository in another notation. It turns off
  `selfHeal` of the root application, and checks the errors.
- **Application namespaces of Argo CD.** Argo CD reads applications only
  in its own namespace, unless `application.namespaces` names more
  namespaces. A person who can make applications in such a namespace
  deploys with each project that lists the namespace in
  `sourceNamespaces`. `applicationsetcontroller.namespaces` does the same
  for ApplicationSets, which make applications. Squared needs neither.
  The guard reads the two keys in the ConfigMap `argocd-cmd-params-cm`,
  and the flags and the environment variables of the application
  controller, the API server and the ApplicationSet controller, which can
  set other values.

  | Setting | Result |
  |---|---|
  | A namespace (also a pattern such as `*` or `/regex/`) other than the namespace of Argo CD that is not in `applicationNamespaces` | Error |
  | A container that reads the variable from another ConfigMap or a Secret, or that has `envFrom` | Error |

  The guard compares the values as text: an operator who permits a
  pattern adds the same text to `applicationNamespaces`. The guard checks
  the applications in all namespaces in any case. The end-to-end test
  sets `application.namespaces` to `default`, adds an application there
  with the platform repository, and checks both errors.
- **ApplicationSets of Argo CD.** An ApplicationSet makes applications
  from a template and the parameters of its generators. Squared makes no
  ApplicationSets. The guard checks the applications that they make, but
  an ApplicationSet makes an application again after a removal, thus the
  guard also checks the ApplicationSets in all namespaces (right `list` on
  `applicationsets` in the cluster).

  | ApplicationSet | Result |
  |---|---|
  | The template, or the template of a generator, has a tenant project, a GitOps repository (`repoPrefixes`), or a label of Squared (`app.kubernetes.io/managed-by`, `squared.dev/managed`, `squared.dev/tenant`) | Error |
  | The project or a repository of the template comes from the parameters (`{{ ... }}`): the guard cannot know the value | Error |
  | The template patch (`templatePatch`) has one of the problems in the rows above (the text after the table tells how the guard reads it) | Error |
  | The guard cannot read the template patch: a line writes text from the parameters (for example `{{ toYaml .x }}`), an action has more than one line, a key comes from the parameters, two branches have the same key, or the YAML is not correct | Error |
  | The template, the template of a generator, or the template patch gives the applications the finalizer `resources-finalizer.argocd.argoproj.io` (also with `/background` or `/foreground`): their removal then removes their objects, also with `preserveResourcesOnDeletion` | Error |
  | `syncPolicy.preserveResourcesOnDeletion` is not `true` | Error |
  | The template, or the template of a generator, has the name of an application of Squared or of the root application, as text | Error |
  | A generator reads a GitOps repository: a Git generator with such a `repoURL` (also from parameters), or an SCM provider or pull request generator for the organization of Squared on Forgejo | Error |
  | An SCM provider or pull request generator with credentials (`tokenRef`, `passwordRef`, `basicAuth`, `bearerToken`, `appSecretName`, `accessTokenRef`) and `insecure: true` or an `http://` API address: the credentials go without TLS checks or as plain text | Error |

  An ApplicationSet of another team writes the project and the
  repositories as text.

  Argo CD applies the template patch to each application after the
  template, also without `goTemplate`. The patch is a Go template that
  writes YAML. The guard reads it for all values of the parameters at the
  same time: it removes the lines with only control actions (`if`,
  `else`, `end`, `range`, `with`, comments and variables), thus it keeps
  the lines of all branches, and it puts a marker in the place of each
  other action. Then it reads the YAML as Argo CD does, and checks the
  name, the labels, the finalizers, the project and the sources as in a
  template. A value with the marker comes from the parameters.

  The guard reads the generators by their structure. Each generator has
  one kind and an optional `selector`. Each kind can have a `template`,
  which changes fields of the template of the ApplicationSet for the
  applications of that generator. The matrix and merge generators have
  generators in them, also in more levels. The guard checks each of these
  templates as the template of the ApplicationSet. The `elements` and
  `elementsYaml` of a list generator and the `values` of a generator are
  parameters, not templates or addresses that the generator reads: the
  checks of the templates find a project or a repository from them. A
  kind that the guard does not know, for example from a newer Argo CD,
  gives a warning: the guard then checks only the applications that the
  generator makes.

  An ApplicationSet that makes an application with the name of an
  existing application takes it over: it writes its template into the
  application and becomes its owner. Argo CD 3.5 does this with each
  `applicationsSync` policy, also `create-only`. Kubernetes then removes
  the application with the ApplicationSet. Without
  `preserveResourcesOnDeletion`, the application keeps the finalizer of
  Argo CD, and the removal also removes the objects of the tenant. With
  it, the ApplicationSet controller removes the finalizer: only the
  application goes, and the root application makes it again. Thus the
  policy does not protect the applications of Squared, but
  `preserveResourcesOnDeletion` protects the workloads of the tenants.
  The guard does not know the names that come from the parameters. After
  a takeover, the root application puts the labels of Squared back, and
  the guard sees the owner.

  The SCM provider and pull request generators send a token from a
  Secret in the namespace of the ApplicationSet to the API of a Git
  server. Without limits, a person who can make an ApplicationSet sends
  each Secret there, for example the Forgejo credentials of Argo CD, to
  each server. Squared needs no SCM generators, thus the development
  environment turns them off. The guard reads the parameters of the
  ApplicationSet controller as for the application namespaces: the keys
  in `argocd-cmd-params-cm`, and the flags and the environment variables
  of the container, which are stronger.

  | Parameter of the ApplicationSet controller | Result |
  |---|---|
  | `applicationsetcontroller.enable.scm.providers` is `false` | No more checks |
  | The SCM generators are on (the default), and `applicationsetcontroller.allowed.scm.providers` is empty (the default): all API addresses are permitted | Error |
  | The SCM generators are on, and an address in `applicationsetcontroller.allowed.scm.providers` does not start with `https://` | Error |
  | The SCM generators are on, and `applicationsetcontroller.enable.tokenref.strict.mode` is not `true` (the default): each Secret of the namespace can be a token, not only Secrets with the label `argocd.argoproj.io/secret-type: scm-creds` | Error |
  | The container reads a parameter from another ConfigMap or a Secret, or has `envFrom` | Error |

  Limit: the list of addresses does not apply to the generators without
  an API address, for example GitHub without `api`. The strict mode
  protects their tokens.

  The policy of the ApplicationSet controller tells what an ApplicationSet
  can do with the applications that it made: `sync` (create, update and
  delete), `create-only`, `create-update` or `create-delete`. Without
  `applicationsetcontroller.policy`, ApplicationSets use `sync`. With it,
  an ApplicationSet can choose another policy only while
  `applicationsetcontroller.enable.policy.override` is on (the default).
  The policy does not stop a takeover (see above). But with updates, an
  ApplicationSet that took over an application of Squared writes its
  template into it again after each repair by the root application, and
  Argo CD can each time remove the objects of the tenant that are not in
  the template. The development environment sets `create-only` and turns
  the override off.

  | Policy | Result |
  |---|---|
  | `applicationsetcontroller.policy` is empty, `sync` or `create-update` | Error |
  | An unknown policy, or a value of the override that is not `true` or `false` | Error |
  | The override is on, and an ApplicationSet chooses `sync` or `create-update` | Error |

  A plugin generator sends a request to a web service and gets the
  parameters from it. The address (`baseUrl`) and the token are in a
  ConfigMap in the namespace of Argo CD (`configMapRef`). The token is a
  reference to a key: `$<key>` in `argocd-secret`, or `$<secret>:<key>`
  in another Secret of the namespace. Argo CD reads each Secret of its
  namespace in this way, also without a label, and sends the value as a
  bearer token. Thus a person who can make a ConfigMap and an
  ApplicationSet can send the client secret of Keycloak or the
  repository credentials of Argo CD to a server. The guard reads the
  ConfigMaps of the plugin generators of all ApplicationSets (right `get`
  on the ConfigMaps of the namespace of Argo CD, which has no secrets in
  ConfigMaps). It reads the ConfigMaps of the cluster decision resource
  generators in the same way.

  | Plugin generator | Result |
  |---|---|
  | The key of the token does not start with `plugin.` | Error |
  | The token is text, not a reference: each person who can read the ConfigMaps of the namespace sees it | Error |
  | A token, and a `baseUrl` with `http://` to a host outside the cluster (not a Service name, `.svc` or `.svc.cluster.local`) | Error |
  | No `baseUrl`, or not an `http` or `https` address | Error |
  | The name of the ConfigMap comes from other generators | Error |
  | The ConfigMap does not exist | Warning |

  A cluster decision resource generator reads the objects of a kind in
  the namespace of Argo CD. Its ConfigMap (`configMapRef`, also in that
  namespace) names the kind (`apiVersion`, `kind`), a list in their
  status (`statusListKey`) and a field of the entries (`matchKey`). Each
  entry whose field has the name of a cluster of Argo CD gives
  parameters: all fields of the entry, and the name and the address of
  the cluster. The controller can read only the kinds that its Role
  permits. The Secrets among them have no status, thus this generator
  cannot send credentials. But the status of an object of Argo CD has
  data from the manifests of the tenants: the status of an application
  lists the objects in its manifests. A tenant could give parameters to
  the ApplicationSet with an object with the name of a cluster.

  | Cluster decision resource generator | Result |
  |---|---|
  | The ConfigMap names a kind of Argo CD (`argoproj.io`) | Error |
  | The name of the ConfigMap comes from other generators | Error |
  | The ConfigMap does not exist, or has no `apiVersion`, `kind`, `statusListKey` or `matchKey`: the generator makes no applications | Warning |

  Limit: the guard does not know the parameters that a plugin generator
  or a cluster decision resource gives. The checks of the templates find
  a project or a repository from the parameters. The end-to-end test
  adds an ApplicationSet with a Git generator for the platform repository,
  a project from the parameters and a pull request generator with a token
  and `insecure: true`, and checks the errors. It turns on the SCM
  generators without limits and checks the errors. The ApplicationSet
  also has a template patch with the finalizer of Argo CD and a tenant
  project in a branch, and a Git generator with a template with a tenant
  project. It adds a plugin generator with a token from a key that is
  not a plugin token and a `baseUrl` without TLS, and one without a
  ConfigMap, and a cluster decision resource generator with the status
  of Argo CD applications, and one with a ConfigMap name from a list, and
  checks the errors and the warning. It lets an
  ApplicationSet take over a test application with the labels of Squared,
  checks the errors, and checks that Kubernetes removes the application
  with the ApplicationSet. It sets the policy `sync`, then turns on the
  override and adds an ApplicationSet with the policy `sync`, and checks
  the errors.
- **Harbor configuration.** Only the Harbor system administrator can read
  the configuration through the API, and a robot account cannot get that
  right. The table `properties` of the Harbor database has all settings,
  also the client secrets. Thus the guard gets a database role,
  `squared_guard`, that can read only the view
  `squared_guard_harbor_config`
  ([`deploy/kind/harbor-guard-view.sql`](../deploy/kind/harbor-guard-view.sql)).
  The view has all keys, and the values only of `auth_mode`,
  `primary_auth_mode`, `oidc_admin_group`, `oidc_groups_claim`,
  `oidc_group_filter` and `http_authproxy_admin_groups`. A test checks that
  the role cannot read the table and that the view has no secret.
- **New Harbor settings.** Harbor keeps only the keys that somebody set,
  thus a missing key tells nothing. The guard compares the keys with the
  settings of Harbor 2.15 that it knows:

  | Change | Result |
  |---|---|
  | A new key with a name like group, team, admin, restrict, claim, role, quota, permission, access or org | Error |
  | A new key with another name | Warning, until an operator adds it to `ignoreFields` |

  An upgrade that renames `oidc_admin_group` also moves the value to a new
  key, thus the guard reports it. The guard protects the administrator
  groups in all authentication modes, because an administrator can change
  the mode. The end-to-end test changes the group through the Harbor API,
  and renames the key in the database. The network policy of the database
  accepts only the Harbor pods and the namespace of the guard.
- **Full group paths.** The groups claim has full paths, for example
  `/squared-admins`, and all services compare the full path. Short names
  are not unique: the worker can make a subgroup with the name
  `squared-admins` under a tenant group. Its path
  `/tenant-acme-owner/squared-admins` does not match `/squared-admins`.
  Never configure a service with a short group name.
- **Rights of the worker in Forgejo.** The Forgejo bot of the worker is a
  Forgejo administrator, because it makes the accounts of members. Protect
  its token as well as the database password.
- **Forgejo accounts belong to Keycloak user IDs.** Forgejo finds the
  account of a Keycloak sign-in by the authentication source and the
  Keycloak user ID (`sub`), which nobody can choose. The worker makes the
  account of each tenant member with that ID before the first sign-in.
  `ACCOUNT_LINKING` is `disabled`: with `auto`, Forgejo 15 gives a first
  sign-in the existing account with the same user name, which is the
  first part of the address with `USERNAME = email`, or with the same
  address, and it does not look at `email_verified`. Then the Keycloak
  user `forgejo-admin@example.com` would get the local administrator
  account `forgejo-admin`, and `alice@a.example` and `alice@b.example`
  would share one account. Now a sign-in whose user name or address
  belongs to another account fails. The worker never takes an existing
  account: if a Forgejo account that belongs to nobody in Keycloak has the
  address of a member, the access sync fails with a clear error, and an
  administrator must check that account. The end-to-end test signs in as
  `e2e-intruder` with the address `dev@e2e-intruder.test`, and as a
  Keycloak user `forgejo-admin` with the address of the local
  administrator, and checks that neither gets an existing account.
- **The guard checks the sign-in and registration settings of Forgejo.**
  Forgejo accounts must come only from Keycloak. The settings are only in
  `app.ini`; Forgejo has no API for them and reads the file only at its
  start. The sidecar `squared forgejo-config` in the Forgejo pod reads the
  sections `[admin]`, `[oauth2_client]`, `[openid]` and `[service]`, and the key
  `INSTALL_LOCK` of `[security]`, at the start of the pod and at each
  request. `[service]` has secrets (for example `RECAPTCHA_SECRET`), thus
  the sidecar serves its key names, but only the values of the sign-in
  and registration keys. Only the namespace of the guard can connect to
  the sidecar. The guard uses the defaults of Forgejo 15 for missing keys:

  | Setting (now or after the next start of Forgejo) | Result |
  |---|---|
  | `ACCOUNT_LINKING = auto`: a sign-in gets the account with the same name or address | Error |
  | Local registration is open: `DISABLE_REGISTRATION` and `ALLOW_ONLY_EXTERNAL_REGISTRATION` are false (the defaults) | Error |
  | `ENABLE_OPENID_SIGNIN` is on (the default before the installation): people sign in with OpenID 2.0 addresses that they add to their accounts, without Keycloak, also after Keycloak disabled them | Error |
  | `ENABLE_OPENID_SIGNUP` is on: anybody with an OpenID 2.0 provider can make an account | Error |
  | `ENABLE_REVERSE_PROXY_AUTHENTICATION` or `..._API` is on: Forgejo accepts the user name in a header from every client | Error |
  | `ALLOWED_HOST_LIST` of `[webhook]` has an entry other than `external` and the hosts in `webhookHosts`: a keyword like `private` or `loopback`, a network, or a host pattern like `*.svc.cluster.local` | Error |
  | `ALLOW_LOCALNETWORKS` of `[migrations]` is on: migrations and mirrors can fetch from private and loopback addresses | Error |
  | A setting that turns off the check of TLS certificates is on: `SKIP_TLS_VERIFY` of `[webhook]`, `[migrations]` or `[email.incoming]`, `FORCE_TRUST_SERVER_CERT` (or its old name `SKIP_VERIFY`) of `[mailer]`, or `MINIO_INSECURE_SKIP_VERIFY` of `[storage]` or another section | Error |
  | `IMPORT_LOCAL_PATHS` of `[security]` is on: Forgejo administrators and users with the flag `allow_import_local` can copy any directory of the Forgejo server into a repository, for example the repositories of other users | Error |
  | `ALLOWED_DOMAINS` of `[migrations]` has a pattern that matches a name in the cluster: `kubernetes`, `kubernetes.default`, `kubernetes.default.svc`, `kubernetes.default.svc.cluster.local`, `localhost`, `node.local` or `metadata.internal` (for example `*`, `*.*` or `*.svc.cluster.local`) | Error |
  | `ENABLE_INTERNAL_SIGNIN` is on (the default): the web form accepts passwords, thus local accounts, and Keycloak users with a Forgejo password, sign in without Keycloak | Error |
  | `manage_password` is not in `EXTERNAL_USER_DISABLE_FEATURES` (or `USER_DISABLED_FEATURES`) of `[admin]` (the default): a Keycloak user can set a Forgejo password | Error |
  | A section changed after the start: the running Forgejo can differ from the file | Error |
  | An unknown key in `[oauth2_client]` or `[openid]`, or in `[service]` or `[admin]` with a name like registration, sign-in, login, auth, link, proxy, OpenID, account, password or feature | Error, until an operator adds `<section>.<key>` to `ignoreFields` |
  | Another unknown key in `[service]` or `[admin]` | Warning, until an operator adds it to `ignoreFields` |
  | `ACCOUNT_LINKING = login` (the default): a person who knows the password of a local account can link it | Warning |
  | `ENABLE_AUTO_REGISTRATION` is off or `ALLOW_ONLY_INTERNAL_REGISTRATION` is on: a Keycloak user without an account gets a page to link a local account with its password | Warning |

  Forgejo reads key names with the exact case, thus `account_linking` is
  an unknown key. The sidecar runs as the Forgejo user with the
  configuration directory read-only, never stops because of the file, and
  has no readiness probe, so that it cannot stop Forgejo. The end-to-end
  test writes `ACCOUNT_LINKING = auto`,
  `ALLOW_ONLY_EXTERNAL_REGISTRATION = false` and
  `ENABLE_INTERNAL_SIGNIN = true` into the file of the running pod and
  checks the errors.
- **Passwords in Forgejo.** The OAuth2 source of Forgejo also accepts the
  Forgejo password of its users, and by default a Keycloak user without a
  password can set one in the account settings. With that password, the
  person signs in without Keycloak, also after Keycloak disabled the
  person, and keeps the administrator flag that the last Keycloak sign-in
  gave. Thus the web form accepts no passwords (`ENABLE_INTERNAL_SIGNIN =
  false`), and Keycloak users cannot set one (`manage_password` in
  `EXTERNAL_USER_DISABLE_FEATURES`). The local administrator and the bots
  of Squared and Argo CD use the API: basic authentication with the
  password (`ENABLE_BASIC_AUTHENTICATION` stays on) or tokens. In an
  emergency, use the API, or the command line in the pod (`forgejo admin
  user ...`). If you turn on the web form for a short time, the guard
  reports an error until you turn it off. The end-to-end test checks that
  the sign-in page has no password form and that `dev` cannot set a
  password. Limit: the guard does not see passwords that Keycloak users
  set before this setting; they work without Keycloak. SSH is off in the
  development environment.
- **Personal access tokens and SSH keys of Forgejo.** A user can make
  tokens in the Forgejo settings, and Forgejo has no setting to prevent
  this. A token works for the API and Git without Keycloak until somebody
  deletes it. The same is true for the SSH keys of a user, and for SSH
  principals: Git over SSH accepts them. A key stays when SSH is off (as in
  the development environment), and works when somebody turns SSH on.
  Thus a credential can outlive the Keycloak user, and it keeps the
  Forgejo administrator flag, which Forgejo changes only at a Keycloak
  sign-in. The guard reads the view `squared_guard_credential_owners` of
  [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql): the
  users of OAuth2 sources that have tokens or keys, with the number and
  the last use of each, never the tokens, their hashes or last
  characters, or the keys and fingerprints, and no local account. Deploy
  keys belong to repositories, not to users, and are not in the view. The
  guard compares each owner of the Keycloak source with the Keycloak
  user:

  | Owner | Result |
  |---|---|
  | The Keycloak user does not exist | Error |
  | Keycloak disabled the user | Error |
  | A Forgejo administrator who is not a direct member of the administrator group of the source (for example `/squared-admins`) | Error |
  | A user of another OAuth2 source | Warning: the guard cannot check it |
  | The Forgejo account is not active or cannot sign in | No message: Forgejo refuses all its requests |

  To correct an error, delete the tokens and keys, or block the Forgejo
  account ("prohibit sign-in"). The guard does not change Forgejo. The
  end-to-end test makes a token for `dev`, disables `dev` in Keycloak, and
  then makes `dev` a Forgejo administrator, and checks the errors. Then it
  adds an SSH key for `dev` and disables `dev` again.
- **Deploy keys of the GitOps repositories.** Squared is the only writer
  of the repositories of its organization (decision D1), and Argo CD
  deploys what they have. A deploy key belongs to a repository, not to a
  user: Git over SSH accepts it without Keycloak, also after the person
  who added it left, and a key with write access pushes without Squared.
  Squared never makes deploy keys; Argo CD reads with a token. The guard
  reads the view `squared_guard_deploy_keys` of
  [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql): the
  repository, the name, the access and the last use of each key, never the
  key or its fingerprint. A key with write access on a repository of the
  organization is an error; a key with read access is a warning (it reads
  all manifests). An operator can accept a key with `<repository>/<key
  name>` in `ignoreFields`. The guard checks the keys also when SSH is
  off. The end-to-end test adds a deploy key with write access to
  `squared/platform` and checks the error.
- **Teams and collaborators of the GitOps repositories.** The same rule
  applies to people: only the bot of the worker (`squared-bot`, in the team
  Owners) may push. The tenant teams and the team `readers` of Argo CD
  give read access. The guard reads the view `squared_guard_repo_access`
  of [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql):
  the members of each team of the organization with the access of the team
  to the code (a team with admin or owner access has it for all units) and
  its repositories, and the collaborators of each repository with their
  access.

  | Access | Result |
  |---|---|
  | A team or a collaboration gives write, admin or owner access to a user who is not in `writers` | Error |
  | A collaboration gives read access | Warning: the person reads outside the membership in Squared |
  | A team gives read access | No message: the tenant teams do this |

  An operator can accept an access with `team:<team>/<user>` or
  `collaborator:<repository>/<user>` in `ignoreFields`. Forgejo site
  administrators can write to all repositories; they are not in this
  check, but the guard checks that their administrator flag comes from
  the Keycloak group (see the credentials above). The end-to-end test gives
  `argocd-reader` write access as a collaborator of `squared/platform`, and
  then through a new team, and checks the errors.
- **Branch protection of the GitOps repositories.** Forgejo site
  administrators have write access to all repositories, and an access
  check cannot stop a mistake that it does not see yet. Thus the worker
  protects the default branch of each GitOps repository with a rule with
  the exact name of the branch (Forgejo uses it before a rule with a
  pattern): only the Forgejo user of the worker can push or merge, the
  rule applies to administrators, and Forgejo refuses all force pushes to
  a protected branch. The worker makes the rule with the repository and
  corrects it at each sync of the tenant. The guard reads the view
  `squared_guard_branch_rules` of
  [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql) and
  reports an error for a repository of the organization without the rule
  (after 5 minutes; a new repository gets a warning first), and for a
  rule that lets other users, teams or deploy keys push or merge, that
  does not apply to administrators, or that stops Squared. Archived
  repositories are not in the check: Forgejo refuses all pushes to them.
  An operator can accept a repository in `ignoreFields`. The end-to-end
  test checks that the local Forgejo administrator cannot write to
  `squared/platform`, that a new tenant repository has the rule, and that
  the guard reports a rule that does not apply to administrators.
- **Hosts that webhooks can reach.** Every Forgejo user can add a
  webhook to a personal repository, and the webhook check of the
  organization does not see it. Forgejo makes the request from inside the
  cluster, thus `ALLOWED_HOST_LIST` decides which services a user can send
  requests to. Without a value, Forgejo permits only hosts on the
  internet (`external`). The webhook to Argo CD needs one host in the
  cluster: the development environment permits exactly
  `argocd-server.argocd.svc.cluster.local` and the internet, not the
  pattern `*.svc.cluster.local`, which would let every user send requests
  to every service, for example the Squared API or Keycloak. The guard
  reads the value from the sidecar (the other values of `[webhook]` stay
  hidden: `PROXY_URL` can have a password), and the end-to-end test checks
  that the webhook to Argo CD still delivers, and writes the pattern into
  `app.ini` and checks the error.
- **Hosts that migrations and mirrors can fetch from.** Every Forgejo
  user can migrate a repository from an address, or mirror one, and
  Forgejo fetches it from inside the cluster. By default
  (`ALLOW_LOCALNETWORKS = false`, no `ALLOWED_DOMAINS`), Forgejo fetches
  only from addresses on the internet. `ALLOW_LOCALNETWORKS = true` adds
  the private and loopback networks, which have all the services of the
  cluster. A pattern in `ALLOWED_DOMAINS` permits a host by its name,
  whatever its address; with the DNS search domains of Kubernetes, even a
  short name like `service.namespace` reaches a service. The guard checks
  each pattern against names in the cluster, as Forgejo matches them. The
  sidecar serves all of `[migrations]` (it has no secrets). The end-to-end
  test adds `ALLOW_LOCALNETWORKS = true` to `app.ini` and checks the error.
  The same is true for the import of directories of the Forgejo server
  (`IMPORT_LOCAL_PATHS` of `[security]`, off by default): the sidecar
  serves this value and `INSTALL_LOCK` of `[security]`, never its secrets,
  and the end-to-end test turns it on in `app.ini` and checks the error.
  The flag `allow_import_local` of a user has no effect while the setting
  is off, thus the guard does not read the flags.
- **Checks of TLS certificates in Forgejo.** Some settings let Forgejo
  connect to a server without the check of its certificate: then a person
  in the network between them can read or change the data, for example
  the content of a mirror, a webhook, a mail with a link to set a password,
  or the files of repositories in object storage. All are off by default.
  The guard reports each of them. `[mailer]` and `[email.incoming]` have
  passwords, and the storage sections have access keys, thus the sidecar
  serves only the keys of the check from them. A value in `[storage]`
  applies to all storage sections, thus the sidecar serves
  `MINIO_INSECURE_SKIP_VERIFY` from every section that has it. The
  end-to-end test turns on `SKIP_TLS_VERIFY` of `[webhook]` in `app.ini`
  and checks the error.
- **Mirrors of the GitOps repositories.** A pull mirror gets its content
  from another repository at intervals: Argo CD would deploy what Squared
  did not write, and Forgejo refuses the pushes of Squared. A push mirror
  copies the content of a repository to another address. Squared never
  makes mirrors. The guard reads the view `squared_guard_mirrors` of
  [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql) and
  reports an error for a repository of the organization that is a pull
  mirror or has a push mirror (archived repositories are not in the
  check). The address of a pull mirror is encrypted, and a push mirror can
  have an SSH key, thus the view has only the host of a push mirror. The
  settings in `[mirror]` of `app.ini` decide only what users can do with
  the repositories that they administer, thus the guard does not check
  them; the addresses of mirrors follow `[migrations]` (see above). The
  end-to-end test adds a push mirror to `squared/platform` in the database
  and checks the error.
- **Webhooks of the GitOps repositories.** A webhook sends the events of
  a repository (commit messages, file names, authors) to its address, and
  Forgejo makes the request from inside the cluster: a webhook can send
  GitOps data out, or send requests to services in the cluster. The only
  webhook that Squared expects is the webhook of the organization to Argo
  CD. The
  guard reads the view `squared_guard_webhooks` of
  [`forgejo-guard-view.sql`](../deploy/kind/forgejo-guard-view.sql): the
  scope, the type, the state and the host of each webhook, and the
  SHA-256 hash of its address. An address can have a secret (a token in
  the path, or a user and a password), thus the view never has the
  address, the secret or the authorization header. The guard compares the
  hash with `webhooks`, which can have `sha256:<hex>` for an address with
  a secret. A webhook of the organization or of one of its repositories,
  or a system or default webhook (they apply to all repositories), with
  another address is an error, also when it is inactive. A missing or
  inactive webhook to an allowed address is a warning: Argo CD then sees
  new commits only when it polls. The end-to-end test adds an inactive
  webhook to `squared/platform` and checks the error.

### Passwords and tokens of Squared

- **Passwords** are hashed with Argon2id (19 MiB memory, 2 iterations, 1
  thread, 16 byte salt). This follows the OWASP recommendation. A password
  has at least 12 characters.
- **Sign-in** gives the same answer and uses the same time when the user
  does not exist. This prevents user enumeration.
- **Rate limit.** The server accepts 10 sign-in attempts in 5 minutes for
  each client address and for each email address.
- **Memory limit for hashes.** At most four Argon2id operations run at the
  same time. Many sign-in requests at the same time thus cannot use all
  memory of the server.
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

1. Run Keycloak with TLS, a production database and `start` (not
   `start-dev`). Remove the `squared-cli` client with the password grant.
2. Turn on a second factor in Keycloak for the administrator group. It
   then protects Squared, Argo CD, Harbor and Forgejo.
3. Put TLS in front of the server (ingress or load balancer). Set
   `SQUARED_HSTS=true` and `SQUARED_TRUST_PROXY=true` if a proxy sets
   `X-Forwarded-For`.
4. Use TLS for PostgreSQL (`sslmode=verify-full`) and RabbitMQ (`amqps://`).
5. Give the worker a Forgejo account that is the owner of the Squared
   organization only. Give Argo CD a separate read-only account.
6. Give the worker a Harbor robot account with system permissions for
   projects and robots, not the `admin` account.
7. Set a webhook secret in Forgejo and in Argo CD.
8. Restrict access to the Argo CD, Forgejo and Harbor web interfaces.
9. Make backups of PostgreSQL and Forgejo. Git and the database together
   are the complete state.
10. With OIDC, do not set the first administrator variables. Without
    OIDC, remove the first administrator password secret after the first
    start, or change the password in the web interface.
