package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
	"squared/internal/forgejo"
	"squared/internal/gitops"
	"squared/internal/harbor"
	"squared/internal/keycloak"
	"squared/internal/kube"
	"squared/internal/render"
	"squared/internal/rest"
)

// FileConfig is the configuration file of the guard (JSON).
type FileConfig struct {
	// Permission is the Keycloak permission with the protected groups.
	Permission string `json:"permission"`
	// Groups are protected in all cases.
	Groups []string `json:"groups"`
	// TenantGroupPrefix and ProjectPrefix must be the same as for the
	// worker. They identify the tenant groups at the places where Squared
	// puts them.
	TenantGroupPrefix string         `json:"tenantGroupPrefix"`
	ProjectPrefix     string         `json:"projectPrefix"`
	Sources           []SourceConfig `json:"sources"`
}

// SourceConfig configures one source. Type selects the fields that apply.
type SourceConfig struct {
	// Type is one of:
	//   argocd-rbac      "g" lines of an Argo CD RBAC ConfigMap (Namespace, Name)
	//   argocd-projects  roles of all Argo CD projects, and the limits of the tenant
	//                    projects (Namespace, RepoPrefixes, ClusterServers)
	//   argocd-applications the sync policies, the sources and the limits of the
	//                    Argo CD applications and ApplicationSets in all
	//                    namespaces, and the namespaces in which Argo CD reads
	//                    them (Namespace, Name
	//                    and RootSource of the root application, RepoPrefixes,
	//                    ClusterServers, AppNamespaces). No groups, only errors.
	//   argocd-repositories the repositories and credential templates of Argo CD, from
	//                    its API, without secrets (URL, PasswordFile with a token,
	//                    RepoPrefixes). No groups, only errors.
	//   argocd-clusters  the clusters of Argo CD, from its API, without secrets (URL,
	//                    PasswordFile with a token, ClusterServers). No groups, only
	//                    errors.
	//   harbor-members   group members of all Harbor projects (URL, User, PasswordFile)
	//   configmap        group paths in a ConfigMap key (Namespace, Name or Selector, Key)
	//   forgejo-init     the init script of the Forgejo Helm chart (Namespace, Name, Key)
	//   forgejo-database the authentication sources in the Forgejo database, through a
	//                    view that has only the group settings (URL, PasswordFile, Name)
	//   harbor-database  the configuration in the Harbor database, through a view
	//                    that has only the group settings (URL, PasswordFile, Name)
	//   keycloak-mappers the mappers of the Keycloak clients that the services use
	//                    (Clients, Audiences, Claims). It gives no groups, only errors.
	//   keycloak-realm   the email settings of the Keycloak realm. It gives no
	//                    groups, only errors.
	//   forgejo-credentials  the owners of personal access tokens and SSH keys in the
	//                    Forgejo database, compared with their Keycloak users (URL,
	//                    PasswordFile, Name, SourceID). No groups, only errors.
	//   forgejo-deploy-keys  the deploy keys of the repositories of the organization Org,
	//                    in the Forgejo database (URL, PasswordFile, Name, Org). No
	//                    groups, only errors.
	//   forgejo-writers  the users with access to the code of the repositories of the
	//                    organization Org through teams or as collaborators, in the
	//                    Forgejo database (URL, PasswordFile, Name, Org, Writers). No
	//                    groups, only errors.
	//   forgejo-branches the protection of the default branch of the repositories of
	//                    the organization Org, in the Forgejo database (URL,
	//                    PasswordFile, Name, Org, Writers). No groups, only errors.
	//   forgejo-webhooks the webhooks that see the repositories of the organization Org,
	//                    in the Forgejo database (URL, PasswordFile, Name, Org,
	//                    Webhooks). No groups, only errors.
	//   forgejo-mirrors  the pull and push mirrors of the repositories of the
	//                    organization Org, in the Forgejo database (URL, PasswordFile,
	//                    Name, Org). No groups, only errors.
	//   forgejo-config   the sign-in, registration and webhook settings in the Forgejo
	//                    app.ini, from the sidecar "squared forgejo-config" (URL,
	//                    WebhookHosts). No groups, only errors.
	Type         string `json:"type"`
	Service      string `json:"service"`
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name,omitempty"`
	Selector     string `json:"selector,omitempty"`
	Key          string `json:"key,omitempty"`
	URL          string `json:"url,omitempty"`
	User         string `json:"user,omitempty"`
	PasswordFile string `json:"passwordFile,omitempty"`
	// Clients, Audiences and Claims are for keycloak-mappers: the client IDs
	// and the custom audiences of the tokens that the services accept, and
	// the claims that they read as groups (default "groups").
	Clients   []string `json:"clients,omitempty"`
	Audiences []string `json:"audiences,omitempty"`
	Claims    []string `json:"claims,omitempty"`
	// SourceID is for forgejo-credentials: the ID of the Keycloak authentication
	// source in Forgejo.
	SourceID int64 `json:"sourceId,omitempty"`
	// Org is for forgejo-deploy-keys, forgejo-writers, forgejo-branches, forgejo-webhooks and forgejo-mirrors: the Forgejo organization of the
	// GitOps repositories (default "squared", as SQUARED_GIT_ORG).
	Org string `json:"org,omitempty"`
	// Writers is for forgejo-writers and forgejo-branches: the users that may push to the
	// repositories of Org, for example the bot of the worker.
	Writers []string `json:"writers,omitempty"`
	// Webhooks is for forgejo-webhooks: the webhook addresses that Squared
	// expects, or "sha256:<hex>" for an address with a secret.
	Webhooks []string `json:"webhooks,omitempty"`
	// RepoPrefixes is for argocd-repositories, argocd-projects and
	// argocd-applications: the addresses of the GitOps
	// repositories, for example "https://git.example/squared/".
	RepoPrefixes []string `json:"repoPrefixes,omitempty"`
	// ClusterServers is for argocd-clusters, argocd-projects and
	// argocd-applications: the API servers that Squared
	// deploys to (SQUARED_CLUSTER_SERVER).
	ClusterServers []string `json:"clusterServers,omitempty"`
	// RootSource is for argocd-applications: the source of the root
	// application (Name).
	RootSource *RootSource `json:"rootSource,omitempty"`
	// AppNamespaces is for argocd-applications: the values of
	// application.namespaces and applicationsetcontroller.namespaces of
	// Argo CD that an operator checked. Squared needs none.
	AppNamespaces []string `json:"applicationNamespaces,omitempty"`
	// WebhookHosts is for forgejo-config: the hosts that ALLOWED_HOST_LIST
	// may have besides "external", for example the host of Argo CD.
	WebhookHosts []string `json:"webhookHosts,omitempty"`
	// IgnoreFields are new fields of a service configuration that an
	// operator checked: they give no rights. The types argocd-rbac (keys),
	// argocd-projects ("spec.<field>" or "roles.<field>"), forgejo-database,
	// harbor-database, keycloak-mappers (mapper types), keycloak-realm
	// (identity providers and user federation), forgejo-config
	// ("<section>.<key>"), forgejo-deploy-keys ("<repository>/<key name>")
	// forgejo-writers ("team:<team>/<user>" or
	// "collaborator:<repository>/<user>"), forgejo-branches (repository)
	// and forgejo-mirrors ("<repository>/<pull or push>") use it.
	IgnoreFields []string `json:"ignoreFields,omitempty"`
}

// LoadFile reads a configuration file.
func LoadFile(path string) (*FileConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg FileConfig
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("guard configuration %s: %w", path, err)
	}
	if len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("guard configuration %s: at least one group in \"groups\" is necessary", path)
	}
	return &cfg, nil
}

// KeycloakReader reads the mappers of the Keycloak clients, the email
// settings of the realm, and users.
type KeycloakReader interface {
	Clients(ctx context.Context) ([]keycloak.ClientMappers, error)
	ClientScopes(ctx context.Context) ([]keycloak.ClientScope, error)
	RealmEmailSettings(ctx context.Context) (*keycloak.RealmEmail, error)
	UserByID(ctx context.Context, id string) (*keycloak.User, error)
	UserGroupPaths(ctx context.Context, id string) ([]string, error)
}

// BuildSources makes the sources of a configuration.
func BuildSources(cfg *FileConfig, kc *kube.Client, idp KeycloakReader) ([]Source, error) {
	tenants := tenantGroups{prefix: cfg.TenantGroupPrefix, projectPrefix: cfg.ProjectPrefix}
	var sources []Source
	for i, sc := range cfg.Sources {
		if sc.Service == "" {
			sc.Service = sc.Type
		}
		switch sc.Type {
		case "argocd-rbac":
			sources = append(sources, &argoRBACSource{sc: sc, kube: kc})
		case "argocd-repositories":
			if sc.URL == "" || sc.PasswordFile == "" || len(sc.RepoPrefixes) == 0 {
				return nil, fmt.Errorf("source %d: argocd-repositories needs url, passwordFile and repoPrefixes", i)
			}
			sources = append(sources, &argoReposSource{sc: sc, rest: argoAPI(sc)})
		case "argocd-clusters":
			if sc.URL == "" || sc.PasswordFile == "" || len(sc.ClusterServers) == 0 {
				return nil, fmt.Errorf("source %d: argocd-clusters needs url, passwordFile and clusterServers", i)
			}
			sources = append(sources, &argoClustersSource{sc: sc, rest: argoAPI(sc)})
		case "argocd-projects":
			if len(sc.RepoPrefixes) == 0 || len(sc.ClusterServers) == 0 {
				return nil, fmt.Errorf("source %d: argocd-projects needs repoPrefixes and clusterServers", i)
			}
			sources = append(sources, &argoProjectsSource{sc: sc, kube: kc, tenants: tenants})
		case "argocd-applications":
			if sc.Name == "" || sc.RootSource == nil || len(sc.RepoPrefixes) == 0 || len(sc.ClusterServers) == 0 {
				return nil, fmt.Errorf("source %d: argocd-applications needs name, rootSource, repoPrefixes and clusterServers", i)
			}
			sources = append(sources, &argoAppsSource{sc: sc, kube: kc})
		case "harbor-members":
			password, err := os.ReadFile(sc.PasswordFile)
			if err != nil {
				return nil, fmt.Errorf("source %d: %w", i, err)
			}
			hb := harbor.New(sc.URL, sc.User, strings.TrimSpace(string(password)), rest.NewHTTPClient(nil))
			sources = append(sources, &harborMembersSource{sc: sc, harbor: hb, tenants: tenants})
		case "configmap":
			if sc.Key == "" || (sc.Name == "") == (sc.Selector == "") {
				return nil, fmt.Errorf("source %d: configmap needs a key and either a name or a selector", i)
			}
			sources = append(sources, &configMapSource{sc: sc, kube: kc})
		case "forgejo-database":
			if sc.URL == "" || sc.PasswordFile == "" {
				return nil, fmt.Errorf("source %d: forgejo-database needs url and passwordFile", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_auth_sources"
			}
			sources = append(sources, &forgejoDatabaseSource{sc: sc})
		case "harbor-database":
			if sc.URL == "" || sc.PasswordFile == "" {
				return nil, fmt.Errorf("source %d: harbor-database needs url and passwordFile", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_harbor_config"
			}
			sources = append(sources, &harborDatabaseSource{sc: sc})
		case "keycloak-mappers":
			if len(sc.Clients) == 0 {
				return nil, fmt.Errorf("source %d: keycloak-mappers needs clients", i)
			}
			if len(sc.Claims) == 0 {
				sc.Claims = []string{"groups"}
			}
			sources = append(sources, &keycloakMappersSource{sc: sc, idp: idp})
		case "keycloak-realm":
			sources = append(sources, &keycloakRealmSource{sc: sc, idp: idp})
		case "forgejo-credentials":
			if sc.URL == "" || sc.PasswordFile == "" || sc.SourceID <= 0 {
				return nil, fmt.Errorf("source %d: forgejo-credentials needs url, passwordFile and sourceId", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_credential_owners"
			}
			sources = append(sources, &forgejoCredentialsSource{sc: sc, idp: idp})
		case "forgejo-deploy-keys":
			if sc.URL == "" || sc.PasswordFile == "" {
				return nil, fmt.Errorf("source %d: forgejo-deploy-keys needs url and passwordFile", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_deploy_keys"
			}
			if sc.Org == "" {
				sc.Org = "squared"
			}
			sources = append(sources, &forgejoDeployKeysSource{sc: sc})
		case "forgejo-writers":
			if sc.URL == "" || sc.PasswordFile == "" || len(sc.Writers) == 0 {
				return nil, fmt.Errorf("source %d: forgejo-writers needs url, passwordFile and writers", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_repo_access"
			}
			if sc.Org == "" {
				sc.Org = "squared"
			}
			sources = append(sources, &forgejoWritersSource{sc: sc})
		case "forgejo-branches":
			if sc.URL == "" || sc.PasswordFile == "" || len(sc.Writers) == 0 {
				return nil, fmt.Errorf("source %d: forgejo-branches needs url, passwordFile and writers", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_branch_rules"
			}
			if sc.Org == "" {
				sc.Org = "squared"
			}
			sources = append(sources, &forgejoBranchesSource{sc: sc})
		case "forgejo-webhooks":
			if sc.URL == "" || sc.PasswordFile == "" {
				return nil, fmt.Errorf("source %d: forgejo-webhooks needs url and passwordFile", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_webhooks"
			}
			if sc.Org == "" {
				sc.Org = "squared"
			}
			sources = append(sources, &forgejoWebhooksSource{sc: sc})
		case "forgejo-mirrors":
			if sc.URL == "" || sc.PasswordFile == "" {
				return nil, fmt.Errorf("source %d: forgejo-mirrors needs url and passwordFile", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_mirrors"
			}
			if sc.Org == "" {
				sc.Org = "squared"
			}
			sources = append(sources, &forgejoMirrorsSource{sc: sc})
		case "forgejo-config":
			if sc.URL == "" {
				return nil, fmt.Errorf("source %d: forgejo-config needs url", i)
			}
			sources = append(sources, &forgejoConfigSource{sc: sc, rest: rest.Client{BaseURL: sc.URL, HTTP: rest.NewHTTPClient(nil)}})
		case "forgejo-init":
			if sc.Key == "" {
				sc.Key = "configure_gitea.sh"
			}
			sources = append(sources, &forgejoInitSource{sc: sc, kube: kc})
		default:
			return nil, fmt.Errorf("source %d: unknown type %q", i, sc.Type)
		}
	}
	return sources, nil
}

// tenantGroups knows the tenant groups and where Squared puts them.
type tenantGroups struct {
	prefix        string // Prefix of the group names, for example "tenant-".
	projectPrefix string // Prefix of the Harbor projects of tenants.
}

func (t tenantGroups) config() gitops.Config { return gitops.Config{GroupPrefix: t.prefix} }

// expected tells if a group is one of the tenant groups of slug.
func (t tenantGroups) expected(slug, group string) bool {
	for _, role := range domain.Roles {
		if t.config().TenantGroupClaim(slug, role) == group {
			return true
		}
	}
	return false
}

type argoRBACSource struct {
	sc   SourceConfig
	kube *kube.Client
}

func (s *argoRBACSource) Name() string { return s.sc.Service + " RBAC" }

// References returns the groups of the "g" lines of the ConfigMap, which
// Argo CD reads at each change. It also checks the keys and the claims in
// the key scopes.
func (s *argoRBACSource) References(ctx context.Context) ([]Reference, []string, error) {
	cm, err := s.kube.GetConfigMap(ctx, s.sc.Namespace, s.sc.Name)
	if err != nil {
		return nil, nil, err
	}
	refs, warnings, err := checkArgoRBAC(cm.Data, s.sc.IgnoreFields)
	for i := range refs {
		refs[i].Service = s.sc.Service
		refs[i].Where = s.sc.Namespace + "/" + s.sc.Name + " " + refs[i].Where
	}
	return refs, warnings, err
}

type argoProjectsSource struct {
	sc      SourceConfig
	kube    *kube.Client
	tenants tenantGroups
}

func (s *argoProjectsSource) Name() string { return s.sc.Service + " projects" }

// References returns the groups of the project roles. The role "member"
// of a tenant project that Squared made, with the tenant groups of that
// tenant, is expected and not protected. Argo CD adds the policies of a
// role to its RBAC without a change, thus a "g" line in them also gives
// rights. Squared never writes such lines.
func (s *argoProjectsSource) References(ctx context.Context) ([]Reference, []string, error) {
	projects, err := s.kube.ListAppProjects(ctx, s.sc.Namespace)
	if err != nil {
		return nil, nil, err
	}
	rules := tenantProjectRules{ClusterServers: s.sc.ClusterServers, RepoPrefixes: s.sc.RepoPrefixes}
	var refs []Reference
	var warnings, problems, tenantProblems []string
	for _, p := range projects {
		w, pr, err := checkArgoProjectFields(p.Raw, s.sc.IgnoreFields)
		if err != nil {
			return nil, nil, fmt.Errorf("project %s: %w", p.Metadata.Name, err)
		}
		for _, m := range w {
			warnings = append(warnings, fmt.Sprintf("Argo CD project %s has %s", p.Metadata.Name, m))
		}
		if len(pr) > 0 {
			problems = append(problems, fmt.Sprintf("project %s: %s", p.Metadata.Name, strings.Join(pr, "; ")))
		}
		squared := p.Metadata.Labels[render.LabelManagedBy] == render.ManagedBy
		slug, isTenant := strings.CutPrefix(p.Metadata.Name, "tenant-")
		switch {
		case isTenant && !squared:
			tenantProblems = append(tenantProblems, fmt.Sprintf("project %s has the name of a tenant project, but Squared did not make it", p.Metadata.Name))
		case isTenant:
			tp, err := checkTenantProject(slug, p.Raw, rules)
			if err != nil {
				return nil, nil, fmt.Errorf("project %s: %w", p.Metadata.Name, err)
			}
			for _, m := range tp {
				tenantProblems = append(tenantProblems, fmt.Sprintf("project %s: %s", p.Metadata.Name, m))
			}
		}
		for _, role := range p.Spec.Roles {
			where := fmt.Sprintf("project %s role %s", p.Metadata.Name, role.Name)
			for _, g := range role.Groups {
				if squared && isTenant && role.Name == "member" && s.tenants.expected(slug, g) {
					continue
				}
				refs = append(refs, Reference{Group: g, Service: s.sc.Service, Where: where})
			}
			for _, r := range argoLineRefs(strings.Join(role.Policies, "\n"), where+" policies") {
				r.Service = s.sc.Service
				refs = append(refs, r)
			}
		}
	}
	// Give all groups that the guard could read, and the error. The guard
	// adds these groups and removes none.
	var errs []string
	if len(tenantProblems) > 0 {
		errs = append(errs, fmt.Sprintf("the tenant projects of Argo CD are not as Squared makes them: %s. The guard keeps the current protection",
			strings.Join(tenantProblems, "; ")))
	}
	if len(problems) > 0 {
		errs = append(errs, fmt.Sprintf("the fields of Argo CD projects changed: %s. The guard keeps the current protection. Update the guard for this Argo CD version",
			strings.Join(problems, " | ")))
	}
	if len(errs) > 0 {
		return refs, warnings, errors.New(joinSentences(errs))
	}
	return refs, warnings, nil
}

type argoAppsSource struct {
	sc   SourceConfig
	kube *kube.Client
}

func (s *argoAppsSource) Name() string { return s.sc.Service + " applications" }

// References checks the namespaces in which Argo CD reads applications,
// and the applications and the ApplicationSets in all namespaces. It gives
// no groups.
func (s *argoAppsSource) References(ctx context.Context) ([]Reference, []string, error) {
	var params map[string]string
	cm, err := s.kube.GetConfigMap(ctx, s.sc.Namespace, argoParamsConfigMap)
	switch {
	case rest.HasStatus(err, http.StatusNotFound):
	case err != nil:
		return nil, nil, err
	default:
		params = cm.Data
	}
	containers := map[string][]kube.Container{}
	for _, w := range argoNamespaceWorkloads {
		c, err := s.kube.GetContainers(ctx, s.sc.Namespace, w.Resource, w.Name)
		if err != nil {
			return nil, nil, err
		}
		containers[w.Name] = c
	}
	values, problems := argoNamespaceValues(params, containers)
	problems = append(problems, checkArgoNamespaces(values, s.sc.Namespace, s.sc.AppNamespaces)...)
	scmProblems := checkArgoSCM(params, containers[argoAppSetController])
	setPolicy, policyProblems := checkArgoSetPolicy(params, containers[argoAppSetController])

	apps, err := s.kube.ListApplicationObjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	rules := argoAppRules{
		tenantProjectRules: tenantProjectRules{ClusterServers: s.sc.ClusterServers, RepoPrefixes: s.sc.RepoPrefixes},
		Namespace:          s.sc.Namespace,
		RootApp:            s.sc.Name,
		Root:               *s.sc.RootSource,
	}
	sets, err := s.kube.ListApplicationSetObjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	names, err := squaredAppNames(apps, rules)
	if err != nil {
		return nil, nil, err
	}
	setProblems, setWarnings, plugins, err := checkArgoAppSets(sets, rules, names, setPolicy)
	if err != nil {
		return nil, nil, err
	}
	// The ApplicationSet controller reads the ConfigMaps of the plugin
	// generators in its own namespace.
	for _, name := range slices.Sorted(maps.Keys(plugins)) {
		users := strings.Join(plugins[name], ", ")
		cm, err := s.kube.GetConfigMap(ctx, s.sc.Namespace, name)
		switch {
		case rest.HasStatus(err, http.StatusNotFound):
			setWarnings = append(setWarnings, fmt.Sprintf("the ApplicationSets %s use the plugin ConfigMap %s, which does not exist", users, name))
		case err != nil:
			return nil, nil, err
		default:
			for _, m := range checkArgoPlugin(cm.Data) {
				setProblems = append(setProblems, fmt.Sprintf("the plugin ConfigMap %s of the ApplicationSets %s: %s", name, users, m))
			}
		}
	}

	var msgs []string
	if len(problems) > 0 {
		msgs = append(msgs, fmt.Sprintf("Argo CD reads applications in other namespaces: %s. The guard keeps the current protection",
			strings.Join(problems, "; ")))
	}
	if len(scmProblems) > 0 {
		msgs = append(msgs, fmt.Sprintf("the SCM settings of the ApplicationSet controller of Argo CD are not safe: %s. The guard keeps the current protection",
			strings.Join(scmProblems, "; ")))
	}
	if len(policyProblems) > 0 {
		msgs = append(msgs, fmt.Sprintf("the policy of the ApplicationSet controller of Argo CD is not safe: %s. The guard keeps the current protection",
			strings.Join(policyProblems, "; ")))
	}
	if len(setProblems) > 0 {
		msgs = append(msgs, fmt.Sprintf("the ApplicationSets of Argo CD can make applications that Squared does not permit: %s. The guard keeps the current protection",
			strings.Join(setProblems, "; ")))
	}
	warnings, err := checkArgoApplications(apps, rules)
	if err != nil {
		msgs = append(msgs, err.Error())
	}
	warnings = append(warnings, setWarnings...)
	if len(msgs) > 0 {
		return nil, warnings, errors.New(joinSentences(msgs))
	}
	return nil, warnings, nil
}

// joinSentences joins messages that end without a full stop into one
// text, with a full stop and a capital letter between them.
func joinSentences(msgs []string) string {
	out := slices.Clone(msgs)
	for i := 1; i < len(out); i++ {
		r, size := utf8.DecodeRuneInString(out[i])
		out[i] = string(unicode.ToUpper(r)) + out[i][size:]
	}
	return strings.Join(out, ". ")
}

type harborMembersSource struct {
	sc      SourceConfig
	harbor  *harbor.Client
	tenants tenantGroups
}

func (s *harborMembersSource) Name() string { return s.sc.Service + " project members" }

// References returns the group members of all projects. The tenant groups
// in the project of the same tenant are expected and not protected.
func (s *harborMembersSource) References(ctx context.Context) ([]Reference, []string, error) {
	projects, err := s.harbor.ProjectNames(ctx)
	if err != nil {
		return nil, nil, err
	}
	var refs []Reference
	for _, project := range projects {
		members, err := s.harbor.Members(ctx, project)
		if err != nil {
			return nil, nil, err
		}
		slug, isTenant := strings.CutPrefix(project, s.tenants.projectPrefix)
		for _, m := range members {
			if m.EntityType != "g" {
				continue
			}
			if isTenant && s.tenants.expected(slug, m.EntityName) {
				continue
			}
			refs = append(refs, Reference{Group: m.EntityName, Service: s.sc.Service, Where: "project " + project})
		}
	}
	return refs, nil, nil
}

type configMapSource struct {
	sc   SourceConfig
	kube *kube.Client
}

func (s *configMapSource) Name() string { return s.sc.Service + " configuration" }

func (s *configMapSource) References(ctx context.Context) ([]Reference, []string, error) {
	var maps []kube.ConfigMap
	if s.sc.Name != "" {
		cm, err := s.kube.GetConfigMap(ctx, s.sc.Namespace, s.sc.Name)
		if err != nil {
			return nil, nil, err
		}
		maps = append(maps, *cm)
	} else {
		var err error
		if maps, err = s.kube.ListConfigMaps(ctx, s.sc.Namespace, s.sc.Selector); err != nil {
			return nil, nil, err
		}
	}
	var refs []Reference
	for _, cm := range maps {
		for _, g := range splitGroups(cm.Data[s.sc.Key]) {
			refs = append(refs, Reference{Group: g, Service: s.sc.Service,
				Where: s.sc.Namespace + "/" + cm.Metadata.Name + " " + s.sc.Key})
		}
	}
	return refs, nil, nil
}

type forgejoInitSource struct {
	sc   SourceConfig
	kube *kube.Client
}

func (s *forgejoInitSource) Name() string { return s.sc.Service + " authentication source" }

func (s *forgejoInitSource) References(ctx context.Context) ([]Reference, []string, error) {
	data, err := s.kube.GetSecretData(ctx, s.sc.Namespace, s.sc.Name)
	if err != nil {
		return nil, nil, err
	}
	var refs []Reference
	for _, g := range forgejoInitGroups(string(data[s.sc.Key])) {
		refs = append(refs, Reference{Group: g, Service: s.sc.Service, Where: s.sc.Namespace + "/" + s.sc.Name + " " + s.sc.Key})
	}
	return refs, nil, nil
}

// forgejoDatabaseSource reads the authentication sources from a view in
// the Forgejo database. It sees changes in the Forgejo web interface,
// which the init script does not have. It checks the field names of each
// source against the fields that the guard knows, so that a renamed field
// after a Forgejo upgrade stops the source instead of hiding a group. See
// deploy/kind/forgejo-guard-view.sql.
type forgejoDatabaseSource struct {
	sc SourceConfig
}

func (s *forgejoDatabaseSource) Name() string { return s.sc.Service + " database" }

// connectDatabase connects to the database of a source with the password
// in the password file.
func connectDatabase(ctx context.Context, sc SourceConfig) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(sc.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	password, err := os.ReadFile(sc.PasswordFile)
	if err != nil {
		return nil, err
	}
	cfg.Password = strings.TrimSpace(string(password))
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

func (s *forgejoDatabaseSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT id, name, type, is_active, cfg_keys,
		coalesce(admin_group, ''), coalesce(restricted_group, ''), coalesce(group_team_map, ''),
		coalesce(group_claim_name, ''), coalesce(required_claim_name, ''), coalesce(required_claim_value, ''),
		coalesce(quota_group_claim_name, ''), coalesce(quota_group_map, '')
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY id")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	defer rows.Close()
	var refs []Reference
	var warnings, problems []string
	for rows.Next() {
		var src forgejoSource
		if err := rows.Scan(&src.ID, &src.Name, &src.Type, &src.Active, &src.Keys,
			&src.AdminGroup, &src.RestrictedGroup, &src.GroupTeamMap,
			&src.GroupClaimName, &src.RequiredClaimName, &src.RequiredClaimValue,
			&src.QuotaGroupClaimName, &src.QuotaGroupMap); err != nil {
			return nil, nil, err
		}
		r, w, err := checkForgejoSource(src, s.sc.IgnoreFields)
		warnings = append(warnings, w...)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for i := range r {
			r[i].Service = s.sc.Service
		}
		refs = append(refs, r...)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(problems) > 0 {
		// Give the groups that the guard could read, and the error. The
		// guard adds these groups and removes none.
		return refs, warnings, fmt.Errorf("%s", strings.Join(problems, " | "))
	}
	return refs, warnings, nil
}

// harborDatabaseSource reads the configuration of Harbor from a view in
// the Harbor database. It sees changes in the Harbor web interface and
// API. It checks the keys against the settings that the guard knows, so
// that a new or renamed setting after a Harbor upgrade stops the source
// instead of hiding a group. See deploy/kind/harbor-guard-view.sql.
type harborDatabaseSource struct {
	sc SourceConfig
}

func (s *harborDatabaseSource) Name() string { return s.sc.Service + " database" }

func (s *harborDatabaseSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SELECT key, coalesce(value, '') FROM "+pgx.Identifier{s.sc.Name}.Sanitize())
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run harbor-guard-view.sql again after a Harbor upgrade): %w", s.sc.Name, err)
	}
	settings := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, nil, err
		}
		settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	refs, warnings, err := checkHarborConfig(settings, s.sc.IgnoreFields)
	for i := range refs {
		refs[i].Service = s.sc.Service
	}
	return refs, warnings, err
}

// keycloakMappersSource checks the mappers of the Keycloak clients that
// the services use. The protection of a group helps only if membership of
// the group is the only way to get it into the groups claim, and the
// services link accounts by the email address. See claimRules.
type keycloakMappersSource struct {
	sc  SourceConfig
	idp KeycloakReader
}

func (s *keycloakMappersSource) Name() string { return s.sc.Service + " client mappers" }

func (s *keycloakMappersSource) References(ctx context.Context) ([]Reference, []string, error) {
	clients, err := s.idp.Clients(ctx)
	if err != nil {
		return nil, nil, err
	}
	scopes, err := s.idp.ClientScopes(ctx)
	if err != nil {
		return nil, nil, err
	}
	warnings, err := checkKeycloakMappers(clients, scopes, mapperRules{
		Clients: s.sc.Clients, Audiences: s.sc.Audiences, Claims: s.sc.Claims, Ignore: s.sc.IgnoreFields})
	return nil, warnings, err
}

// keycloakRealmSource checks that a person can get only an email address
// that the person owns. See checkRealmEmail.
type keycloakRealmSource struct {
	sc  SourceConfig
	idp KeycloakReader
}

func (s *keycloakRealmSource) Name() string { return s.sc.Service + " realm email settings" }

func (s *keycloakRealmSource) References(ctx context.Context) ([]Reference, []string, error) {
	settings, err := s.idp.RealmEmailSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	warnings, err := checkRealmEmail(*settings, s.sc.IgnoreFields)
	return nil, warnings, err
}

// forgejoConfigSource reads the sign-in, registration and webhook settings
// of Forgejo from the sidecar "squared forgejo-config" in the pod of
// Forgejo. Forgejo has no API for them, and they are only in app.ini. See
// checkForgejoSettings.
type forgejoConfigSource struct {
	sc   SourceConfig
	rest rest.Client
}

func (s *forgejoConfigSource) Name() string { return s.sc.Service + " settings" }

func (s *forgejoConfigSource) References(ctx context.Context) ([]Reference, []string, error) {
	var rep forgejo.ConfigReport
	if err := s.rest.Do(ctx, http.MethodGet, "", nil, &rep); err != nil {
		return nil, nil, err
	}
	if rep.Start == nil && rep.Error == "" {
		return nil, nil, fmt.Errorf("the answer of %s is not a report of the Forgejo settings", s.sc.URL)
	}
	warnings, err := checkForgejoSettings(rep, s.sc.IgnoreFields, s.sc.WebhookHosts)
	return nil, warnings, err
}

// forgejoCredentialsSource reads the owners of personal access tokens and
// SSH keys from a view in the Forgejo database, and compares them with
// their Keycloak users. See checkForgejoCredentials and
// deploy/kind/forgejo-guard-view.sql.
type forgejoCredentialsSource struct {
	sc  SourceConfig
	idp KeycloakReader
}

func (s *forgejoCredentialsSource) Name() string { return s.sc.Service + " credentials" }

func (s *forgejoCredentialsSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	// The administrator group of the Keycloak source: Forgejo sets the
	// administrator flag from it at each Keycloak sign-in.
	var adminGroup string
	err = conn.QueryRow(ctx, "SELECT coalesce(admin_group, '') FROM squared_guard_auth_sources WHERE id = $1 AND type = $2",
		s.sc.SourceID, forgejoOAuthType).Scan(&adminGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("Forgejo has no OAuth2 authentication source with the ID %d (sourceId)", s.sc.SourceID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read squared_guard_auth_sources: %w", err)
	}
	rows, err := conn.Query(ctx, `SELECT user_name, login_source, login_name, is_admin, is_active, prohibit_login,
		tokens, tokens_last_used_unix, ssh_keys, ssh_keys_last_used_unix
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY user_name")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var owners []forgejoCredentialOwner
	for rows.Next() {
		var o forgejoCredentialOwner
		var tokensUsed, keysUsed int64
		if err := rows.Scan(&o.UserName, &o.LoginSource, &o.LoginName, &o.IsAdmin, &o.IsActive, &o.ProhibitLogin,
			&o.Tokens, &tokensUsed, &o.SSHKeys, &keysUsed); err != nil {
			return nil, nil, err
		}
		o.TokensLastUsed, o.SSHKeysLastUsed = time.Unix(tokensUsed, 0), time.Unix(keysUsed, 0)
		owners = append(owners, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoCredentials(ctx, owners, s.sc.SourceID, adminGroup, keycloakUserStateFrom(s.idp))
	return nil, warnings, err
}

// forgejoDeployKeysSource reads the deploy keys from a view in the Forgejo
// database. See checkForgejoDeployKeys and deploy/kind/forgejo-guard-view.sql.
type forgejoDeployKeysSource struct {
	sc SourceConfig
}

func (s *forgejoDeployKeysSource) Name() string { return s.sc.Service + " deploy keys" }

func (s *forgejoDeployKeysSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT owner_name, repo_name, key_name, mode, created_unix, last_used_unix
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY owner_name, repo_name, key_name")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var keys []forgejoDeployKey
	for rows.Next() {
		var k forgejoDeployKey
		var created, used int64
		if err := rows.Scan(&k.Owner, &k.Repo, &k.Name, &k.Mode, &created, &used); err != nil {
			return nil, nil, err
		}
		k.Created, k.LastUsed = time.Unix(created, 0), time.Unix(used, 0)
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoDeployKeys(keys, s.sc.Org, s.sc.IgnoreFields)
	return nil, warnings, err
}

// forgejoWritersSource reads the team and collaborator access to the code
// of the repositories from a view in the Forgejo database. See
// checkForgejoWriters and deploy/kind/forgejo-guard-view.sql.
type forgejoWritersSource struct {
	sc SourceConfig
}

func (s *forgejoWritersSource) Name() string { return s.sc.Service + " repository access" }

func (s *forgejoWritersSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT org_name, kind, via, user_name, mode, repos
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY org_name, kind, via, user_name")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var access []forgejoRepoAccess
	for rows.Next() {
		var a forgejoRepoAccess
		if err := rows.Scan(&a.Org, &a.Kind, &a.Via, &a.User, &a.Mode, &a.Repos); err != nil {
			return nil, nil, err
		}
		access = append(access, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoWriters(access, s.sc.Org, s.sc.Writers, s.sc.IgnoreFields)
	return nil, warnings, err
}

// forgejoBranchesSource reads the protection of the default branches from a
// view in the Forgejo database. See checkForgejoBranches and
// deploy/kind/forgejo-guard-view.sql.
type forgejoBranchesSource struct {
	sc SourceConfig
}

func (s *forgejoBranchesSource) Name() string { return s.sc.Service + " branch protection" }

func (s *forgejoBranchesSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT owner_name, repo_name, branch, repo_created_unix, archived, protected, can_push,
		enable_whitelist, push_users, push_teams, push_deploy_keys, enable_merge_whitelist, merge_users, merge_teams, apply_to_admins
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY owner_name, repo_name")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var rules []forgejoBranchRule
	for rows.Next() {
		var r forgejoBranchRule
		var created int64
		if err := rows.Scan(&r.Owner, &r.Repo, &r.Branch, &created, &r.Archived, &r.Protected, &r.CanPush,
			&r.EnableWhitelist, &r.PushUsers, &r.PushTeams, &r.PushDeployKeys, &r.EnableMergeWhitelist,
			&r.MergeUsers, &r.MergeTeams, &r.ApplyToAdmins); err != nil {
			return nil, nil, err
		}
		r.RepoCreated = time.Unix(created, 0)
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoBranches(rules, s.sc.Org, s.sc.Writers, s.sc.IgnoreFields, time.Now())
	return nil, warnings, err
}

// forgejoWebhooksSource reads the webhooks from a view in the Forgejo
// database. See checkForgejoWebhooks and deploy/kind/forgejo-guard-view.sql.
type forgejoWebhooksSource struct {
	sc SourceConfig
}

func (s *forgejoWebhooksSource) Name() string { return s.sc.Service + " webhooks" }

func (s *forgejoWebhooksSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT scope, owner_name, repo_name, type, active, url_sha256, url_host
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY scope, owner_name, repo_name, url_host")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var hooks []forgejoWebhook
	for rows.Next() {
		var h forgejoWebhook
		if err := rows.Scan(&h.Scope, &h.Owner, &h.Repo, &h.Type, &h.Active, &h.URLSHA256, &h.URLHost); err != nil {
			return nil, nil, err
		}
		hooks = append(hooks, h)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoWebhooks(hooks, s.sc.Org, s.sc.Webhooks)
	return nil, warnings, err
}

// forgejoMirrorsSource reads the mirrors from a view in the Forgejo
// database. See checkForgejoMirrors and deploy/kind/forgejo-guard-view.sql.
type forgejoMirrorsSource struct {
	sc SourceConfig
}

func (s *forgejoMirrorsSource) Name() string { return s.sc.Service + " mirrors" }

func (s *forgejoMirrorsSource) References(ctx context.Context) ([]Reference, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := connectDatabase(ctx, s.sc)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT owner_name, repo_name, archived, kind, remote_host
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY owner_name, repo_name, kind, remote_host")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var mirrors []forgejoMirror
	for rows.Next() {
		var m forgejoMirror
		if err := rows.Scan(&m.Owner, &m.Repo, &m.Archived, &m.Kind, &m.RemoteHost); err != nil {
			return nil, nil, err
		}
		mirrors = append(mirrors, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoMirrors(mirrors, s.sc.Org, s.sc.IgnoreFields)
	return nil, warnings, err
}

// argoReposSource reads the repositories and credential templates of Argo
// CD from its API, with a token of a local account that can only read
// them. The API never shows passwords, tokens or keys. See
// checkArgoRepositories.
type argoReposSource struct {
	sc   SourceConfig
	rest rest.Client
}

func (s *argoReposSource) Name() string { return s.sc.Service + " repositories" }

func (s *argoReposSource) References(ctx context.Context) ([]Reference, []string, error) {
	var repos []argoRepo
	type repoList struct {
		Items []struct {
			Repo     string `json:"repo"`
			Insecure bool   `json:"insecure"`
		} `json:"items"`
	}
	type credsList struct {
		Items []struct {
			URL string `json:"url"`
		} `json:"items"`
	}
	var rl repoList
	if err := s.rest.Do(ctx, http.MethodGet, "/api/v1/repositories", nil, &rl); err != nil {
		return nil, nil, err
	}
	for _, r := range rl.Items {
		repos = append(repos, argoRepo{URL: r.Repo, Insecure: r.Insecure})
	}
	var cl credsList
	if err := s.rest.Do(ctx, http.MethodGet, "/api/v1/repocreds", nil, &cl); err != nil {
		return nil, nil, err
	}
	for _, c := range cl.Items {
		repos = append(repos, argoRepo{URL: c.URL, Template: true})
	}
	// Argo CD answers 501 for the write repositories when the source
	// hydrator is off. Then nothing pushes with write credentials.
	var wl repoList
	hydratorOn := true
	err := s.rest.Do(ctx, http.MethodGet, "/api/v1/write-repositories", nil, &wl)
	switch {
	case rest.HasStatus(err, http.StatusNotImplemented):
		hydratorOn = false
	case err != nil:
		return nil, nil, err
	}
	if hydratorOn {
		for _, r := range wl.Items {
			repos = append(repos, argoRepo{URL: r.Repo, Write: true, Insecure: r.Insecure})
		}
		var wcl credsList
		if err := s.rest.Do(ctx, http.MethodGet, "/api/v1/write-repocreds", nil, &wcl); err != nil {
			return nil, nil, err
		}
		for _, c := range wcl.Items {
			repos = append(repos, argoRepo{URL: c.URL, Template: true, Write: true})
		}
	}
	warnings, err := checkArgoRepositories(repos, s.sc.RepoPrefixes, hydratorOn)
	return nil, warnings, err
}

// argoAPI returns a client of the API of Argo CD that sends the token of
// the password file. It reads the file at each request, thus a new token
// works without a restart.
func argoAPI(sc SourceConfig) rest.Client {
	file := sc.PasswordFile
	return rest.Client{BaseURL: strings.TrimRight(sc.URL, "/"), HTTP: rest.NewHTTPClient(nil),
		Auth: func(r *http.Request) error {
			token, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
			return nil
		}}
}

// argoClustersSource reads the clusters of Argo CD from its API, with a
// token of a local account that can only read them. The API never shows
// the tokens, the keys or the settings of exec providers. See
// checkArgoClusters.
type argoClustersSource struct {
	sc   SourceConfig
	rest rest.Client
}

func (s *argoClustersSource) Name() string { return s.sc.Service + " clusters" }

func (s *argoClustersSource) References(ctx context.Context) ([]Reference, []string, error) {
	var list struct {
		Items []struct {
			Server  string `json:"server"`
			Name    string `json:"name"`
			Project string `json:"project"`
			Config  struct {
				TLSClientConfig struct {
					Insecure bool `json:"insecure"`
				} `json:"tlsClientConfig"`
			} `json:"config"`
		} `json:"items"`
	}
	if err := s.rest.Do(ctx, http.MethodGet, "/api/v1/clusters", nil, &list); err != nil {
		return nil, nil, err
	}
	clusters := make([]argoCluster, 0, len(list.Items))
	for _, c := range list.Items {
		clusters = append(clusters, argoCluster{Server: c.Server, Name: c.Name, Project: c.Project, Insecure: c.Config.TLSClientConfig.Insecure})
	}
	warnings, err := checkArgoClusters(clusters, s.sc.ClusterServers)
	return nil, warnings, err
}
