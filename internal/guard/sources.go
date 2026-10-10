package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

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
	//   argocd-projects  roles of all Argo CD projects (Namespace)
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
	//   forgejo-tokens   the owners of personal access tokens in the Forgejo database,
	//                    compared with their Keycloak users (URL, PasswordFile, Name,
	//                    SourceID). No groups, only errors.
	//   forgejo-config   the sign-in and registration settings in the Forgejo app.ini,
	//                    from the sidecar "squared forgejo-config" (URL). No groups,
	//                    only errors.
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
	// SourceID is for forgejo-tokens: the ID of the Keycloak authentication
	// source in Forgejo.
	SourceID int64 `json:"sourceId,omitempty"`
	// IgnoreFields are new fields of a service configuration that an
	// operator checked: they give no rights. The types argocd-rbac (keys),
	// argocd-projects ("spec.<field>" or "roles.<field>"), forgejo-database,
	// harbor-database, keycloak-mappers (mapper types), keycloak-realm
	// (identity providers and user federation) and forgejo-config
	// ("<section>.<key>") use it.
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
		case "argocd-projects":
			sources = append(sources, &argoProjectsSource{sc: sc, kube: kc, tenants: tenants})
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
		case "forgejo-tokens":
			if sc.URL == "" || sc.PasswordFile == "" || sc.SourceID <= 0 {
				return nil, fmt.Errorf("source %d: forgejo-tokens needs url, passwordFile and sourceId", i)
			}
			if sc.Name == "" {
				sc.Name = "squared_guard_token_owners"
			}
			sources = append(sources, &forgejoTokensSource{sc: sc, idp: idp})
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
	var refs []Reference
	var warnings, problems []string
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
	if len(problems) > 0 {
		// Give all groups that the guard could read, and the error. The
		// guard adds these groups and removes none.
		return refs, warnings, fmt.Errorf("the fields of Argo CD projects changed: %s. The guard keeps the current protection. Update the guard for this Argo CD version",
			strings.Join(problems, " | "))
	}
	return refs, warnings, nil
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

// forgejoConfigSource reads the sign-in and registration settings of
// Forgejo from the sidecar "squared forgejo-config" in the pod of Forgejo.
// Forgejo has no API for them, and they are only in app.ini. See
// checkForgejoSignIn.
type forgejoConfigSource struct {
	sc   SourceConfig
	rest rest.Client
}

func (s *forgejoConfigSource) Name() string { return s.sc.Service + " sign-in settings" }

func (s *forgejoConfigSource) References(ctx context.Context) ([]Reference, []string, error) {
	var rep forgejo.ConfigReport
	if err := s.rest.Do(ctx, http.MethodGet, "", nil, &rep); err != nil {
		return nil, nil, err
	}
	if rep.Start == nil && rep.Error == "" {
		return nil, nil, fmt.Errorf("the answer of %s is not a report of the sign-in settings", s.sc.URL)
	}
	warnings, err := checkForgejoSignIn(rep, s.sc.IgnoreFields)
	return nil, warnings, err
}

// forgejoTokensSource reads the owners of personal access tokens from a
// view in the Forgejo database, and compares them with their Keycloak
// users. See checkForgejoTokens and deploy/kind/forgejo-guard-view.sql.
type forgejoTokensSource struct {
	sc  SourceConfig
	idp KeycloakReader
}

func (s *forgejoTokensSource) Name() string { return s.sc.Service + " access tokens" }

func (s *forgejoTokensSource) References(ctx context.Context) ([]Reference, []string, error) {
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
	rows, err := conn.Query(ctx, `SELECT user_name, login_source, login_name, is_admin, is_active, prohibit_login, tokens, last_used_unix
		FROM `+pgx.Identifier{s.sc.Name}.Sanitize()+" ORDER BY user_name")
	if err != nil {
		return nil, nil, fmt.Errorf("read %s (run forgejo-guard-view.sql again after a Forgejo upgrade): %w", s.sc.Name, err)
	}
	var owners []forgejoTokenOwner
	for rows.Next() {
		var o forgejoTokenOwner
		var lastUsed int64
		if err := rows.Scan(&o.UserName, &o.LoginSource, &o.LoginName, &o.IsAdmin, &o.IsActive, &o.ProhibitLogin, &o.Tokens, &lastUsed); err != nil {
			return nil, nil, err
		}
		o.LastUsed = time.Unix(lastUsed, 0)
		owners = append(owners, o)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	warnings, err := checkForgejoTokens(ctx, owners, s.sc.SourceID, adminGroup, keycloakUserStateFrom(s.idp))
	return nil, warnings, err
}
