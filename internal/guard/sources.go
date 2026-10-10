package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"squared/internal/domain"
	"squared/internal/gitops"
	"squared/internal/harbor"
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
	Type         string `json:"type"`
	Service      string `json:"service"`
	Namespace    string `json:"namespace,omitempty"`
	Name         string `json:"name,omitempty"`
	Selector     string `json:"selector,omitempty"`
	Key          string `json:"key,omitempty"`
	URL          string `json:"url,omitempty"`
	User         string `json:"user,omitempty"`
	PasswordFile string `json:"passwordFile,omitempty"`
	// IgnoreFields are new fields of a service configuration that an
	// operator checked: they give no rights. Only forgejo-database uses it.
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

// BuildSources makes the sources of a configuration.
func BuildSources(cfg *FileConfig, kc *kube.Client) ([]Source, error) {
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

var argoPolicyKey = regexp.MustCompile(`^policy(\..+)?\.csv$`)

func (s *argoRBACSource) References(ctx context.Context) ([]Reference, []string, error) {
	cm, err := s.kube.GetConfigMap(ctx, s.sc.Namespace, s.sc.Name)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		if argoPolicyKey.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var refs []Reference
	for _, k := range keys {
		groups, others := argoRBACGroups(cm.Data[k])
		where := s.sc.Namespace + "/" + s.sc.Name + " " + k
		for _, g := range groups {
			refs = append(refs, Reference{Group: g, Service: s.sc.Service, Where: where})
		}
		// A short name never matches the full paths of the groups claim.
		// Report it, because it shows a configuration error.
		for _, o := range others {
			if !strings.Contains(o, "@") {
				refs = append(refs, Reference{Group: o, Service: s.sc.Service, Where: where})
			}
		}
	}
	return refs, nil, nil
}

type argoProjectsSource struct {
	sc      SourceConfig
	kube    *kube.Client
	tenants tenantGroups
}

func (s *argoProjectsSource) Name() string { return s.sc.Service + " projects" }

// References returns the groups of the project roles. The role "member"
// of a tenant project that Squared made, with the tenant groups of that
// tenant, is expected and not protected.
func (s *argoProjectsSource) References(ctx context.Context) ([]Reference, []string, error) {
	projects, err := s.kube.ListAppProjects(ctx, s.sc.Namespace)
	if err != nil {
		return nil, nil, err
	}
	var refs []Reference
	for _, p := range projects {
		squared := p.Metadata.Labels[render.LabelManagedBy] == render.ManagedBy
		slug, isTenant := strings.CutPrefix(p.Metadata.Name, "tenant-")
		for _, role := range p.Spec.Roles {
			for _, g := range role.Groups {
				if squared && isTenant && role.Name == "member" && s.tenants.expected(slug, g) {
					continue
				}
				refs = append(refs, Reference{Group: g, Service: s.sc.Service,
					Where: fmt.Sprintf("project %s role %s", p.Metadata.Name, role.Name)})
			}
		}
	}
	return refs, nil, nil
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

func (s *forgejoDatabaseSource) References(ctx context.Context) ([]Reference, []string, error) {
	cfg, err := pgx.ParseConfig(s.sc.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse url: %w", err)
	}
	password, err := os.ReadFile(s.sc.PasswordFile)
	if err != nil {
		return nil, nil, err
	}
	cfg.Password = strings.TrimSpace(string(password))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
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
