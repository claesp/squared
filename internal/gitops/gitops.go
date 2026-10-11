// Package gitops makes the files of the platform repository. Argo CD
// applies this repository with the root application. The repository has
// one directory for each tenant:
//
//	tenants/<tenant>/appproject.json        Argo CD project that limits the tenant
//	tenants/<tenant>/<env>/*.json           Namespace, quota, limits, network policy, RBAC
//	tenants/<tenant>/apps/<env>-<name>.json Argo CD application of one deployment
//
// The manifests of the deployments are in the repository of the tenant:
//
//	<env>/<deployment>/*.json
package gitops

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"squared/internal/domain"
	"squared/internal/render"
)

// Config tells where things are.
type Config struct {
	Org                  string // Forgejo organization of all Squared repositories.
	PlatformRepo         string // Name of the platform repository.
	Branch               string // Branch that Argo CD follows.
	RepoURLBase          string // Base URL of Forgejo that Argo CD uses, for example "http://forgejo-http.forgejo:3000".
	ArgoNamespace        string // Namespace of Argo CD.
	ClusterServer        string // Destination cluster of the applications.
	WorkerServiceAccount string // Service account of the worker.
	WorkerNamespace      string // Namespace of the worker.
	IngressNamespace     string // Namespace of the ingress controller. Its pods can connect to tenant pods.
	// GroupPrefix is the start of the identity provider groups of tenants.
	// The groups are "<prefix><tenant>-<role>". Default "tenant-".
	GroupPrefix string
}

// TenantGroupClaim returns the value of a tenant group in the groups claim:
// the full path of the top-level group. Services compare full paths,
// because short names are not unique.
func (c Config) TenantGroupClaim(slug string, role domain.Role) string {
	return "/" + c.TenantGroup(slug, role)
}

// TenantGroup returns the name of the identity provider group of the
// members of a tenant with a role.
func (c Config) TenantGroup(slug string, role domain.Role) string {
	prefix := c.GroupPrefix
	if prefix == "" {
		prefix = "tenant-"
	}
	return prefix + slug + "-" + string(role)
}

// Labels.
const (
	LabelManaged      = "squared.dev/managed"
	LabelDeploymentID = "squared.dev/deployment-id"
	LabelEnvironment  = "squared.dev/environment"
	AnnotRevision     = "squared.dev/revision"
)

// TenantRepo returns the name of the repository of a tenant.
func TenantRepo(slug string) string { return "tenant-" + slug }

// ProjectName returns the name of the Argo CD project of a tenant.
func ProjectName(slug string) string { return "tenant-" + slug }

// RepoURL returns the clone URL of a repository for Argo CD.
func (c Config) RepoURL(repo string) string {
	return c.RepoURLBase + "/" + c.Org + "/" + repo + ".git"
}

// TenantDir returns the directory of a tenant in the platform repository.
func TenantDir(slug string) string { return "tenants/" + slug }

// AppsDir returns the directory of the applications of a tenant.
func AppsDir(slug string) string { return TenantDir(slug) + "/apps" }

// AppFile returns the path of the application of a deployment.
func AppFile(slug, env, name string) string {
	return AppsDir(slug) + "/" + env + "-" + name + ".json"
}

// DeploymentDir returns the directory of a deployment in the tenant
// repository.
func DeploymentDir(env, name string) string { return env + "/" + name }

type meta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Finalizers  []string          `json:"finalizers,omitempty"`
}

type object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   meta   `json:"metadata"`
	Spec       any    `json:"spec,omitempty"`
	// Fields of objects without "spec".
	Rules    any `json:"rules,omitempty"`
	RoleRef  any `json:"roleRef,omitempty"`
	Subjects any `json:"subjects,omitempty"`
}

func labels(slug string) map[string]string {
	return map[string]string{
		render.LabelManagedBy: render.ManagedBy,
		render.LabelTenant:    slug,
	}
}

// TenantFiles returns the files of a tenant in the platform repository,
// without the applications.
func (c Config) TenantFiles(t *domain.Tenant, envs []domain.Environment) (map[string][]byte, error) {
	objs := map[string]object{
		path.Join(TenantDir(t.Slug), "appproject.json"): c.appProject(t, envs),
	}
	for _, e := range envs {
		dir := path.Join(TenantDir(t.Slug), e.Name)
		objs[dir+"/namespace.json"] = c.namespace(t, e)
		objs[dir+"/resourcequota.json"] = c.quota(t, e)
		objs[dir+"/limitrange.json"] = c.limitRange(t, e)
		objs[dir+"/networkpolicy.json"] = c.networkPolicy(t, e)
		objs[dir+"/role.json"] = c.workerRole(t, e)
		objs[dir+"/rolebinding.json"] = c.workerRoleBinding(t, e)
	}
	return marshalAll(objs)
}

func marshalAll(objs map[string]object) (map[string][]byte, error) {
	files := make(map[string][]byte, len(objs))
	for p, o := range objs {
		b, err := render.Marshal(o)
		if err != nil {
			return nil, err
		}
		files[p] = b
	}
	return files, nil
}

// TenantScope selects the files that TenantFiles owns: all files of the
// tenant except the applications.
func TenantScope(slug string) func(string) bool {
	dir, apps := TenantDir(slug)+"/", AppsDir(slug)+"/"
	return func(p string) bool {
		return strings.HasPrefix(p, dir) && !strings.HasPrefix(p, apps)
	}
}

// GroupKind is a Kubernetes kind with its API group.
type GroupKind struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
}

// TenantKinds are the only kinds that a tenant project permits: the kinds
// that the renderer makes. Tenants cannot make cluster objects. This also
// protects against bad commits to the tenant repository.
var TenantKinds = []GroupKind{
	{"", "ServiceAccount"},
	{"", "Service"},
	{"", "ConfigMap"},
	{"apps", "Deployment"},
	{"batch", "CronJob"},
	{"networking.k8s.io", "Ingress"},
	{"policy", "PodDisruptionBudget"},
}

// MemberRole is the name of the role of the tenant members in a tenant
// project.
const MemberRole = "member"

// MemberPolicies are the policies of the role "member" of a tenant
// project: read access to the applications and the logs.
func MemberPolicies(slug string) []string {
	p := ProjectName(slug)
	return []string{
		"p, proj:" + p + ":" + MemberRole + ", applications, get, " + p + "/*, allow",
		"p, proj:" + p + ":" + MemberRole + ", logs, get, " + p + "/*, allow",
	}
}

func (c Config) appProject(t *domain.Tenant, envs []domain.Environment) object {
	type dest struct {
		Server    string `json:"server"`
		Namespace string `json:"namespace"`
	}
	dests := make([]dest, 0, len(envs))
	for _, e := range envs {
		dests = append(dests, dest{Server: c.ClusterServer, Namespace: e.Namespace})
	}
	sort.Slice(dests, func(i, j int) bool { return dests[i].Namespace < dests[j].Namespace })
	return object{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "AppProject",
		Metadata:   meta{Name: ProjectName(t.Slug), Namespace: c.ArgoNamespace, Labels: labels(t.Slug)},
		Spec: map[string]any{
			"description":  "Squared tenant " + t.Slug,
			"sourceRepos":  []string{c.RepoURL(TenantRepo(t.Slug))},
			"destinations": dests,
			// Tenants cannot make cluster objects, and only the kinds that
			// the renderer makes. This also protects against bad commits
			// to the tenant repository.
			// Members of the tenant can read their applications and the
			// logs of their pods in Argo CD. Changes go through Squared.
			"roles": []map[string]any{{
				"name":        MemberRole,
				"description": "Members of the tenant. Read access to applications and logs.",
				"policies":    MemberPolicies(t.Slug),
				"groups": []string{
					c.TenantGroupClaim(t.Slug, domain.RoleOwner),
					c.TenantGroupClaim(t.Slug, domain.RoleDeveloper),
					c.TenantGroupClaim(t.Slug, domain.RoleViewer),
				},
			}},
			"clusterResourceWhitelist":   []GroupKind{},
			"namespaceResourceWhitelist": TenantKinds,
		},
	}
}

func (c Config) namespace(t *domain.Tenant, e domain.Environment) object {
	l := labels(t.Slug)
	l[LabelEnvironment] = e.Name
	// Pod Security Admission blocks pods that do not agree with the
	// "restricted" standard.
	l["pod-security.kubernetes.io/enforce"] = "restricted"
	l["pod-security.kubernetes.io/enforce-version"] = "latest"
	l["pod-security.kubernetes.io/warn"] = "restricted"
	l["pod-security.kubernetes.io/audit"] = "restricted"
	return object{
		APIVersion: "v1",
		Kind:       "Namespace",
		Metadata: meta{Name: e.Namespace, Labels: l, Annotations: map[string]string{
			// Argo CD does not delete a namespace when its file is removed.
			// An administrator removes it after a check. This protects
			// against data loss after a bad commit.
			"argocd.argoproj.io/sync-options": "Prune=false",
			// Make the namespace before the objects in it.
			"argocd.argoproj.io/sync-wave": "-1",
		}},
	}
}

func (c Config) quota(t *domain.Tenant, e domain.Environment) object {
	return object{
		APIVersion: "v1",
		Kind:       "ResourceQuota",
		Metadata:   meta{Name: "squared", Namespace: e.Namespace, Labels: labels(t.Slug)},
		Spec: map[string]any{"hard": map[string]string{
			"requests.cpu":           t.Quota.CPU,
			"requests.memory":        t.Quota.Memory,
			"limits.memory":          t.Quota.Memory,
			"pods":                   strconv.Itoa(t.Quota.Pods),
			"services.loadbalancers": "0",
			"services.nodeports":     "0",
			"persistentvolumeclaims": "0",
		}},
	}
}

func (c Config) limitRange(t *domain.Tenant, e domain.Environment) object {
	return object{
		APIVersion: "v1",
		Kind:       "LimitRange",
		Metadata:   meta{Name: "squared", Namespace: e.Namespace, Labels: labels(t.Slug)},
		Spec: map[string]any{"limits": []map[string]any{{
			"type":           "Container",
			"default":        map[string]string{"memory": "256Mi"},
			"defaultRequest": map[string]string{"cpu": "50m", "memory": "128Mi"},
			"max":            map[string]string{"memory": "8Gi"},
		}}},
	}
}

// networkPolicy allows connections from pods in the same namespace and
// from the ingress controller. It blocks connections from other tenants.
func (c Config) networkPolicy(t *domain.Tenant, e domain.Environment) object {
	from := []map[string]any{
		{"podSelector": map[string]any{}},
	}
	if c.IngressNamespace != "" {
		from = append(from, map[string]any{"namespaceSelector": map[string]any{
			"matchLabels": map[string]string{"kubernetes.io/metadata.name": c.IngressNamespace},
		}})
	}
	return object{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "NetworkPolicy",
		Metadata:   meta{Name: "squared-isolation", Namespace: e.Namespace, Labels: labels(t.Slug)},
		Spec: map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []string{"Ingress"},
			"ingress":     []map[string]any{{"from": from}},
		},
	}
}

// workerRole lets the worker write the image pull secret. Kubernetes RBAC
// cannot limit "create" to one name, thus the role allows secrets in this
// namespace only.
func (c Config) workerRole(t *domain.Tenant, e domain.Environment) object {
	return object{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "Role",
		Metadata:   meta{Name: "squared-worker", Namespace: e.Namespace, Labels: labels(t.Slug)},
		Rules: []map[string]any{{
			"apiGroups": []string{""},
			"resources": []string{"secrets"},
			"verbs":     []string{"get", "create", "patch"},
		}},
	}
}

func (c Config) workerRoleBinding(t *domain.Tenant, e domain.Environment) object {
	return object{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "RoleBinding",
		Metadata:   meta{Name: "squared-worker", Namespace: e.Namespace, Labels: labels(t.Slug)},
		RoleRef:    map[string]string{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "squared-worker"},
		Subjects: []map[string]string{{
			"kind": "ServiceAccount", "name": c.WorkerServiceAccount, "namespace": c.WorkerNamespace,
		}},
	}
}

// MarkerPath is the path of a manifest that is always in the platform
// repository. Argo CD refuses an automatic sync that removes all objects
// of an application. Without this manifest, the removal of the last
// tenant would be such a sync. With it, the protection stays for a
// repository that somebody empties by mistake.
const MarkerPath = "tenants/platform.json"

// MarkerFile returns the content of the file at MarkerPath.
func (c Config) MarkerFile() ([]byte, error) {
	return render.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "squared-platform",
			"namespace": c.ArgoNamespace,
			"labels":    map[string]string{render.LabelManagedBy: render.ManagedBy},
		},
		"data": map[string]string{
			"description": "Squared writes the files of the platform repository. This object is always there.",
		},
	})
}

// AppInput describes the application of a deployment.
type AppInput struct {
	Tenant     *domain.Tenant
	Deployment *domain.Deployment
	Commit     string // Commit in the tenant repository.
	Revision   int
}

// ApplicationFile returns the Argo CD application of a deployment. The
// application follows one fixed commit, not a branch. Thus a deployment
// changes only when Squared commits a new application file, and each
// revision maps to exactly one commit.
func (c Config) ApplicationFile(in AppInput) (string, []byte, error) {
	if len(in.Commit) != 40 {
		return "", nil, fmt.Errorf("commit %q is not a full SHA-1", in.Commit)
	}
	d := in.Deployment
	l := labels(in.Tenant.Slug)
	l[LabelManaged] = "true"
	l[LabelDeploymentID] = d.ID
	l[render.LabelDeployment] = d.Name
	l[LabelEnvironment] = d.Environment
	obj := object{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
		Metadata: meta{
			Name:        d.AppName,
			Namespace:   c.ArgoNamespace,
			Labels:      l,
			Annotations: map[string]string{AnnotRevision: strconv.Itoa(in.Revision)},
			// Argo CD removes the objects of the application before it
			// removes the application.
			Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
		},
		Spec: map[string]any{
			"project": ProjectName(in.Tenant.Slug),
			"source": map[string]any{
				"repoURL":        c.RepoURL(TenantRepo(in.Tenant.Slug)),
				"targetRevision": in.Commit,
				"path":           DeploymentDir(d.Environment, d.Name),
			},
			"destination": map[string]string{"server": c.ClusterServer, "namespace": d.Namespace},
			"syncPolicy": map[string]any{
				"automated":   map[string]bool{"prune": true, "selfHeal": true},
				"syncOptions": []string{"CreateNamespace=false", "PruneLast=true"},
				"retry": map[string]any{
					"limit":   5,
					"backoff": map[string]any{"duration": "10s", "factor": 2, "maxDuration": "3m"},
				},
			},
		},
	}
	b, err := render.Marshal(obj)
	if err != nil {
		return "", nil, err
	}
	return AppFile(in.Tenant.Slug, d.Environment, d.Name), b, nil
}
