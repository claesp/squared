package render

// This file has the small part of the Kubernetes API types that Squared
// writes. Typed structs give a fixed field order, thus the JSON output is
// the same for the same input.

// ObjectMeta is the metadata of an object.
type ObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Object is the common part of all top level objects.
type Object struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
}

// ServiceAccount gives the pods an identity without API access.
type ServiceAccount struct {
	Object
	AutomountServiceAccountToken bool `json:"automountServiceAccountToken"`
}

// Deployment runs a set of equal pods.
type Deployment struct {
	Object
	Spec DeploymentSpec `json:"spec"`
}

// DeploymentSpec is the specification of a Deployment.
type DeploymentSpec struct {
	Replicas             int             `json:"replicas"`
	RevisionHistoryLimit int             `json:"revisionHistoryLimit"`
	Selector             LabelSelector   `json:"selector"`
	Template             PodTemplateSpec `json:"template"`
}

// LabelSelector selects objects with labels.
type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

// PodTemplateSpec is the template for new pods.
type PodTemplateSpec struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
}

// PodSpec is the specification of a pod.
type PodSpec struct {
	ServiceAccountName           string                     `json:"serviceAccountName"`
	AutomountServiceAccountToken bool                       `json:"automountServiceAccountToken"`
	EnableServiceLinks           bool                       `json:"enableServiceLinks"`
	RestartPolicy                string                     `json:"restartPolicy,omitempty"`
	SecurityContext              PodSecurityContext         `json:"securityContext"`
	ImagePullSecrets             []LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Containers                   []Container                `json:"containers"`
	Volumes                      []Volume                   `json:"volumes,omitempty"`
	TopologySpreadConstraints    []TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
}

// PodSecurityContext is the security context of a pod.
type PodSecurityContext struct {
	RunAsNonRoot   bool            `json:"runAsNonRoot"`
	RunAsUser      *int64          `json:"runAsUser,omitempty"`
	RunAsGroup     *int64          `json:"runAsGroup,omitempty"`
	FSGroup        *int64          `json:"fsGroup,omitempty"`
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

// SeccompProfile selects a seccomp profile.
type SeccompProfile struct {
	Type string `json:"type"`
}

// LocalObjectReference refers to an object in the same namespace.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// TopologySpreadConstraint spreads pods over nodes.
type TopologySpreadConstraint struct {
	MaxSkew           int           `json:"maxSkew"`
	TopologyKey       string        `json:"topologyKey"`
	WhenUnsatisfiable string        `json:"whenUnsatisfiable"`
	LabelSelector     LabelSelector `json:"labelSelector"`
}

// Container is one container of a pod.
type Container struct {
	Name            string                   `json:"name"`
	Image           string                   `json:"image"`
	ImagePullPolicy string                   `json:"imagePullPolicy"`
	Command         []string                 `json:"command,omitempty"`
	Args            []string                 `json:"args,omitempty"`
	Env             []EnvVar                 `json:"env,omitempty"`
	Ports           []ContainerPort          `json:"ports,omitempty"`
	Resources       ResourceRequirements     `json:"resources"`
	SecurityContext ContainerSecurityContext `json:"securityContext"`
	ReadinessProbe  *Probe                   `json:"readinessProbe,omitempty"`
	LivenessProbe   *Probe                   `json:"livenessProbe,omitempty"`
	VolumeMounts    []VolumeMount            `json:"volumeMounts,omitempty"`
}

// EnvVar is an environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContainerPort is a port that a container opens.
type ContainerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

// ResourceRequirements are the resource requests and limits.
type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// ContainerSecurityContext is the security context of a container.
type ContainerSecurityContext struct {
	AllowPrivilegeEscalation bool         `json:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   bool         `json:"readOnlyRootFilesystem"`
	RunAsNonRoot             bool         `json:"runAsNonRoot"`
	Capabilities             Capabilities `json:"capabilities"`
}

// Capabilities are Linux capabilities.
type Capabilities struct {
	Drop []string `json:"drop"`
}

// Probe checks a container.
type Probe struct {
	HTTPGet             *HTTPGetAction `json:"httpGet,omitempty"`
	TCPSocket           *TCPAction     `json:"tcpSocket,omitempty"`
	InitialDelaySeconds int            `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       int            `json:"periodSeconds"`
	TimeoutSeconds      int            `json:"timeoutSeconds"`
	FailureThreshold    int            `json:"failureThreshold"`
}

// HTTPGetAction is an HTTP check.
type HTTPGetAction struct {
	Path string `json:"path"`
	Port string `json:"port"`
}

// TCPAction is a TCP connection check.
type TCPAction struct {
	Port string `json:"port"`
}

// VolumeMount mounts a volume in a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

// Volume is a volume of a pod. Squared uses only emptyDir volumes.
type Volume struct {
	Name     string         `json:"name"`
	EmptyDir *EmptyDirLimit `json:"emptyDir,omitempty"`
}

// EmptyDirLimit is an emptyDir volume with a size limit.
type EmptyDirLimit struct {
	SizeLimit string `json:"sizeLimit"`
}

// Service gives pods a stable network address.
type Service struct {
	Object
	Spec ServiceSpec `json:"spec"`
}

// ServiceSpec is the specification of a Service.
type ServiceSpec struct {
	Type     string            `json:"type"`
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
}

// ServicePort is a port of a Service.
type ServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort string `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

// Ingress sends HTTP traffic from outside the cluster to a Service.
type Ingress struct {
	Object
	Spec IngressSpec `json:"spec"`
}

// IngressSpec is the specification of an Ingress.
type IngressSpec struct {
	IngressClassName string        `json:"ingressClassName,omitempty"`
	TLS              []IngressTLS  `json:"tls,omitempty"`
	Rules            []IngressRule `json:"rules"`
}

// IngressTLS selects the certificate for hosts.
type IngressTLS struct {
	Hosts      []string `json:"hosts"`
	SecretName string   `json:"secretName"`
}

// IngressRule routes the traffic of a host.
type IngressRule struct {
	Host string           `json:"host"`
	HTTP IngressRuleValue `json:"http"`
}

// IngressRuleValue has the paths of a rule.
type IngressRuleValue struct {
	Paths []IngressPath `json:"paths"`
}

// IngressPath routes one path.
type IngressPath struct {
	Path     string         `json:"path"`
	PathType string         `json:"pathType"`
	Backend  IngressBackend `json:"backend"`
}

// IngressBackend is the target of a path.
type IngressBackend struct {
	Service IngressServiceBackend `json:"service"`
}

// IngressServiceBackend is a Service port.
type IngressServiceBackend struct {
	Name string             `json:"name"`
	Port ServiceBackendPort `json:"port"`
}

// ServiceBackendPort selects a port by name.
type ServiceBackendPort struct {
	Name string `json:"name"`
}

// PodDisruptionBudget keeps pods available during maintenance.
type PodDisruptionBudget struct {
	Object
	Spec PDBSpec `json:"spec"`
}

// PDBSpec is the specification of a PodDisruptionBudget.
type PDBSpec struct {
	MaxUnavailable int           `json:"maxUnavailable"`
	Selector       LabelSelector `json:"selector"`
}

// CronJob runs a job on a schedule.
type CronJob struct {
	Object
	Spec CronJobSpec `json:"spec"`
}

// CronJobSpec is the specification of a CronJob.
type CronJobSpec struct {
	Schedule                   string          `json:"schedule"`
	TimeZone                   string          `json:"timeZone"`
	ConcurrencyPolicy          string          `json:"concurrencyPolicy"`
	SuccessfulJobsHistoryLimit int             `json:"successfulJobsHistoryLimit"`
	FailedJobsHistoryLimit     int             `json:"failedJobsHistoryLimit"`
	JobTemplate                JobTemplateSpec `json:"jobTemplate"`
}

// JobTemplateSpec is the template for new jobs.
type JobTemplateSpec struct {
	Spec JobSpec `json:"spec"`
}

// JobSpec is the specification of a job.
type JobSpec struct {
	BackoffLimit            int             `json:"backoffLimit"`
	ActiveDeadlineSeconds   int             `json:"activeDeadlineSeconds"`
	TTLSecondsAfterFinished int             `json:"ttlSecondsAfterFinished"`
	Template                PodTemplateSpec `json:"template"`
}
