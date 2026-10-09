// Package render changes a package revision into Kubernetes manifests.
//
// The output is deterministic: the same input gives the same bytes. All
// workloads get secure settings that tenants cannot change. They agree
// with the "restricted" Pod Security Standard.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"squared/internal/catalog"
	"squared/internal/domain"
)

// Component types.
const (
	TypeWeb    = "web"    // Deployment with a Service and an optional Ingress.
	TypeWorker = "worker" // Deployment without network access from other pods.
	TypeCron   = "cron"   // CronJob.
)

// PullSecretName is the name of the image pull secret that the worker
// puts in each tenant namespace.
const PullSecretName = "squared-registry"

// Labels and annotations.
const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	LabelInstance   = "app.kubernetes.io/instance"
	LabelComponent  = "app.kubernetes.io/component"
	LabelName       = "app.kubernetes.io/name"
	LabelPartOf     = "app.kubernetes.io/part-of"
	LabelVersion    = "app.kubernetes.io/version"
	LabelTenant     = "squared.dev/tenant"
	LabelDeployment = "squared.dev/deployment"
	ManagedBy       = "squared"
)

// Input is everything that the renderer needs.
type Input struct {
	Tenant       string
	Namespace    string
	Deployment   string
	Package      *catalog.Package
	Values       map[string]any    // Resolved values, as Package.Resolve returns.
	Images       map[string]string // Image parameter name to full reference with digest.
	IngressClass string            // Optional ingress class for web components.
	IngressTLS   bool              // Add a TLS section to each Ingress.
}

// File is one manifest file.
type File struct {
	Path    string
	Content []byte
}

// Render returns the manifests of a deployment, ordered by path.
func Render(in Input) ([]File, error) {
	values := make(map[string]any, len(in.Values))
	for k, v := range in.Values {
		values[k] = v
	}
	for _, prm := range in.Package.Parameters {
		if prm.Type != catalog.TypeImage {
			continue
		}
		if _, ok := values[prm.Name]; !ok {
			continue
		}
		ref, ok := in.Images[prm.Name]
		if !ok || !strings.Contains(ref, "@sha256:") {
			return nil, fmt.Errorf("image of parameter %q has no resolved digest", prm.Name)
		}
		values[prm.Name] = ref
	}

	var objects []any
	for _, c := range in.Package.Components {
		raw, err := in.Package.Settings(c, values)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.Name, err)
		}
		s, err := decodeSettings(c.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.Name, err)
		}
		r := componentRenderer{in: in, comp: c, s: s}
		objs, err := r.render()
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.Name, err)
		}
		objects = append(objects, objs...)
	}

	files := make([]File, 0, len(objects))
	seen := map[string]bool{}
	for _, obj := range objects {
		meta := objectOf(obj)
		path := strings.ToLower(meta.Kind) + "-" + meta.Metadata.Name + ".json"
		if seen[path] {
			return nil, fmt.Errorf("two objects have the file name %s", path)
		}
		seen[path] = true
		b, err := Marshal(obj)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: path, Content: b})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// Marshal encodes an object as indented JSON with a final new line.
func Marshal(obj any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return buf.Bytes(), nil
}

func objectOf(obj any) Object {
	switch o := obj.(type) {
	case *ServiceAccount:
		return o.Object
	case *Deployment:
		return o.Object
	case *Service:
		return o.Object
	case *Ingress:
		return o.Object
	case *PodDisruptionBudget:
		return o.Object
	case *CronJob:
		return o.Object
	}
	panic(fmt.Sprintf("render: unknown object type %T", obj))
}

// settings are the settings of all component types. decodeSettings
// rejects settings that the component type does not use.
type settings struct {
	Image      string            `json:"image"`
	Replicas   *int              `json:"replicas,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	CPU        string            `json:"cpu,omitempty"`
	Memory     string            `json:"memory,omitempty"`
	RunAsUser  *int64            `json:"run_as_user,omitempty"`
	TmpSize    string            `json:"tmp_size,omitempty"`
	Port       int               `json:"port,omitempty"`
	HealthPath string            `json:"health_path,omitempty"`
	Host       string            `json:"host,omitempty"`
	Schedule   string            `json:"schedule,omitempty"`
	TimeZone   string            `json:"time_zone,omitempty"`
}

var allowedSettings = map[string][]string{
	TypeWeb:    {"image", "replicas", "command", "args", "env", "cpu", "memory", "run_as_user", "tmp_size", "port", "health_path", "host"},
	TypeWorker: {"image", "replicas", "command", "args", "env", "cpu", "memory", "run_as_user", "tmp_size"},
	TypeCron:   {"image", "command", "args", "env", "cpu", "memory", "run_as_user", "tmp_size", "schedule", "time_zone"},
}

var (
	hostPattern     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	cronField       = regexp.MustCompile(`^[0-9A-Za-z*/,-]+$`)
	timeZonePattern = regexp.MustCompile(`^[A-Za-z]+(/[A-Za-z0-9_+-]+)*$`)
)

// Check checks the settings of a component. It has the type
// catalog.Checker.
func Check(componentType string, raw map[string]json.RawMessage) error {
	_, err := decodeSettings(componentType, raw)
	return err
}

func decodeSettings(componentType string, raw map[string]json.RawMessage) (*settings, error) {
	allowed, ok := allowedSettings[componentType]
	if !ok {
		return nil, fmt.Errorf("unknown component type %q", componentType)
	}
	for key := range raw {
		if !contains(allowed, key) {
			return nil, fmt.Errorf("setting %q is not valid for type %s", key, componentType)
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s settings
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	applyDefaults(componentType, &s)
	if err := s.validate(componentType); err != nil {
		return nil, err
	}
	return &s, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func applyDefaults(componentType string, s *settings) {
	if s.Replicas == nil && componentType != TypeCron {
		one := 1
		s.Replicas = &one
	}
	if s.CPU == "" {
		s.CPU = "100m"
	}
	if s.Memory == "" {
		s.Memory = "128Mi"
	}
	if s.TmpSize == "" {
		s.TmpSize = "64Mi"
	}
	if componentType == TypeWeb {
		if s.Port == 0 {
			s.Port = 8080
		}
		if s.HealthPath == "" {
			s.HealthPath = "/"
		}
	}
	if componentType == TypeCron && s.TimeZone == "" {
		s.TimeZone = "Etc/UTC"
	}
}

func (s *settings) validate(componentType string) error {
	var v domain.ValidationError
	if s.Image == "" {
		v.Add("image", "is required")
	}
	if s.Replicas != nil && (*s.Replicas < 0 || *s.Replicas > 50) {
		v.Add("replicas", "must be from 0 to 50")
	}
	if !domain.ValidQuantity(s.CPU) {
		v.Add("cpu", "must be a Kubernetes quantity")
	}
	if !domain.ValidQuantity(s.Memory) {
		v.Add("memory", "must be a Kubernetes quantity")
	}
	if !domain.ValidQuantity(s.TmpSize) {
		v.Add("tmp_size", "must be a Kubernetes quantity")
	}
	if s.RunAsUser != nil && (*s.RunAsUser < 1 || *s.RunAsUser > 2147483647) {
		v.Add("run_as_user", "must be a user ID other than 0")
	}
	for name := range s.Env {
		if strings.HasPrefix(name, "SQUARED_") || (componentType == TypeWeb && name == "PORT") {
			v.Add("env", "the name "+name+" is reserved")
		}
	}
	if componentType == TypeWeb {
		if s.Port < 1024 || s.Port > 65535 {
			v.Add("port", "must be from 1024 to 65535")
		}
		if !strings.HasPrefix(s.HealthPath, "/") || strings.ContainsAny(s.HealthPath, " \t\n") {
			v.Add("health_path", "must be a path that starts with /")
		}
		if s.Host != "" && (len(s.Host) > 253 || !hostPattern.MatchString(s.Host)) {
			v.Add("host", "must be a DNS name in lowercase")
		}
	}
	if componentType == TypeCron {
		fields := strings.Fields(s.Schedule)
		ok := len(fields) == 5
		for _, f := range fields {
			ok = ok && cronField.MatchString(f)
		}
		if !ok {
			v.Add("schedule", "must be a cron schedule with five fields, for example \"0 3 * * *\"")
		}
		if !timeZonePattern.MatchString(s.TimeZone) {
			v.Add("time_zone", "must be an IANA time zone, for example Europe/Stockholm")
		}
	}
	return v.Err()
}

type componentRenderer struct {
	in   Input
	comp catalog.Component
	s    *settings
}

func (r *componentRenderer) name() string { return r.in.Deployment + "-" + r.comp.Name }

func (r *componentRenderer) selector() map[string]string {
	return map[string]string{
		LabelInstance:  r.in.Deployment,
		LabelComponent: r.comp.Name,
	}
}

func (r *componentRenderer) labels() map[string]string {
	return map[string]string{
		LabelName:       r.comp.Name,
		LabelInstance:   r.in.Deployment,
		LabelComponent:  r.comp.Name,
		LabelPartOf:     r.in.Package.Name,
		LabelVersion:    r.in.Package.Version,
		LabelManagedBy:  ManagedBy,
		LabelTenant:     r.in.Tenant,
		LabelDeployment: r.in.Deployment,
	}
}

func (r *componentRenderer) meta(kind, apiVersion string) Object {
	return Object{
		APIVersion: apiVersion,
		Kind:       kind,
		Metadata:   ObjectMeta{Name: r.name(), Namespace: r.in.Namespace, Labels: r.labels()},
	}
}

func (r *componentRenderer) render() ([]any, error) {
	if len(r.name()) > 63 {
		return nil, fmt.Errorf("object name %q has more than 63 characters", r.name())
	}
	objs := []any{
		&ServiceAccount{Object: r.meta("ServiceAccount", "v1")},
	}
	switch r.comp.Type {
	case TypeWeb:
		objs = append(objs, r.deployment(), r.service())
		if r.s.Host != "" {
			objs = append(objs, r.ingress())
		}
		if *r.s.Replicas > 1 {
			objs = append(objs, r.pdb())
		}
	case TypeWorker:
		objs = append(objs, r.deployment())
		if *r.s.Replicas > 1 {
			objs = append(objs, r.pdb())
		}
	case TypeCron:
		objs = append(objs, r.cronJob())
	default:
		return nil, fmt.Errorf("unknown component type %q", r.comp.Type)
	}
	return objs, nil
}

func (r *componentRenderer) podSpec(restartPolicy string) PodSpec {
	s := r.s
	c := Container{
		Name:            r.comp.Name,
		Image:           s.Image,
		ImagePullPolicy: "IfNotPresent",
		Command:         s.Command,
		Args:            s.Args,
		Env:             r.env(),
		Resources: ResourceRequirements{
			Requests: map[string]string{"cpu": s.CPU, "memory": s.Memory},
			Limits:   map[string]string{"memory": s.Memory},
		},
		SecurityContext: ContainerSecurityContext{
			AllowPrivilegeEscalation: false,
			ReadOnlyRootFilesystem:   true,
			RunAsNonRoot:             true,
			Capabilities:             Capabilities{Drop: []string{"ALL"}},
		},
		VolumeMounts: []VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
	}
	if r.comp.Type == TypeWeb {
		c.Ports = []ContainerPort{{Name: "http", ContainerPort: s.Port, Protocol: "TCP"}}
		c.ReadinessProbe = &Probe{
			HTTPGet: &HTTPGetAction{Path: s.HealthPath, Port: "http"}, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3,
		}
		c.LivenessProbe = &Probe{
			HTTPGet: &HTTPGetAction{Path: s.HealthPath, Port: "http"}, InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 6,
		}
	}
	spec := PodSpec{
		ServiceAccountName:           r.name(),
		AutomountServiceAccountToken: false,
		EnableServiceLinks:           false,
		RestartPolicy:                restartPolicy,
		SecurityContext: PodSecurityContext{
			RunAsNonRoot:   true,
			RunAsUser:      s.RunAsUser,
			RunAsGroup:     s.RunAsUser,
			FSGroup:        s.RunAsUser,
			SeccompProfile: &SeccompProfile{Type: "RuntimeDefault"},
		},
		ImagePullSecrets: []LocalObjectReference{{Name: PullSecretName}},
		Containers:       []Container{c},
		Volumes:          []Volume{{Name: "tmp", EmptyDir: &EmptyDirLimit{SizeLimit: s.TmpSize}}},
	}
	if s.Replicas != nil && *s.Replicas > 1 {
		spec.TopologySpreadConstraints = []TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: "ScheduleAnyway",
			LabelSelector: LabelSelector{MatchLabels: r.selector()},
		}}
	}
	return spec
}

// env returns the environment variables, ordered by name. Squared adds
// variables that tell the program where it runs.
func (r *componentRenderer) env() []EnvVar {
	names := make([]string, 0, len(r.s.Env))
	for k := range r.s.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	env := make([]EnvVar, 0, len(names)+3)
	for _, k := range names {
		env = append(env, EnvVar{Name: k, Value: r.s.Env[k]})
	}
	env = append(env,
		EnvVar{Name: "SQUARED_COMPONENT", Value: r.comp.Name},
		EnvVar{Name: "SQUARED_DEPLOYMENT", Value: r.in.Deployment},
		EnvVar{Name: "SQUARED_TENANT", Value: r.in.Tenant},
	)
	if r.comp.Type == TypeWeb {
		env = append(env, EnvVar{Name: "PORT", Value: strconv.Itoa(r.s.Port)})
	}
	return env
}

func (r *componentRenderer) podTemplate(restartPolicy string) PodTemplateSpec {
	return PodTemplateSpec{
		Metadata: ObjectMeta{Name: r.name(), Labels: r.labels()},
		Spec:     r.podSpec(restartPolicy),
	}
}

func (r *componentRenderer) deployment() *Deployment {
	tmpl := r.podTemplate("")
	tmpl.Metadata.Name = "" // Kubernetes makes the pod names.
	return &Deployment{
		Object: r.meta("Deployment", "apps/v1"),
		Spec: DeploymentSpec{
			Replicas:             *r.s.Replicas,
			RevisionHistoryLimit: 5,
			Selector:             LabelSelector{MatchLabels: r.selector()},
			Template:             tmpl,
		},
	}
}

func (r *componentRenderer) service() *Service {
	return &Service{
		Object: r.meta("Service", "v1"),
		Spec: ServiceSpec{
			Type:     "ClusterIP",
			Selector: r.selector(),
			Ports:    []ServicePort{{Name: "http", Port: 80, TargetPort: "http", Protocol: "TCP"}},
		},
	}
}

func (r *componentRenderer) ingress() *Ingress {
	ing := &Ingress{
		Object: r.meta("Ingress", "networking.k8s.io/v1"),
		Spec: IngressSpec{
			IngressClassName: r.in.IngressClass,
			Rules: []IngressRule{{
				Host: r.s.Host,
				HTTP: IngressRuleValue{Paths: []IngressPath{{
					Path:     "/",
					PathType: "Prefix",
					Backend:  IngressBackend{Service: IngressServiceBackend{Name: r.name(), Port: ServiceBackendPort{Name: "http"}}},
				}}},
			}},
		},
	}
	if r.in.IngressTLS {
		ing.Spec.TLS = []IngressTLS{{Hosts: []string{r.s.Host}, SecretName: r.name() + "-tls"}}
	}
	return ing
}

func (r *componentRenderer) pdb() *PodDisruptionBudget {
	return &PodDisruptionBudget{
		Object: r.meta("PodDisruptionBudget", "policy/v1"),
		Spec:   PDBSpec{MaxUnavailable: 1, Selector: LabelSelector{MatchLabels: r.selector()}},
	}
}

func (r *componentRenderer) cronJob() *CronJob {
	tmpl := r.podTemplate("Never")
	tmpl.Metadata.Name = ""
	return &CronJob{
		Object: r.meta("CronJob", "batch/v1"),
		Spec: CronJobSpec{
			Schedule:                   r.s.Schedule,
			TimeZone:                   r.s.TimeZone,
			ConcurrencyPolicy:          "Forbid",
			SuccessfulJobsHistoryLimit: 3,
			FailedJobsHistoryLimit:     3,
			JobTemplate: JobTemplateSpec{Spec: JobSpec{
				BackoffLimit:            2,
				ActiveDeadlineSeconds:   3600,
				TTLSecondsAfterFinished: 86400,
				Template:                tmpl,
			}},
		},
	}
}
