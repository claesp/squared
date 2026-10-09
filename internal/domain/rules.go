package domain

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Role is the permission level of a user in a tenant.
type Role string

// Roles, from the most to the least permissions.
const (
	RoleOwner     Role = "owner"     // Can also change members and environments.
	RoleDeveloper Role = "developer" // Can change deployments.
	RoleViewer    Role = "viewer"    // Can only read.
)

// Roles lists the valid roles.
var Roles = []Role{RoleOwner, RoleDeveloper, RoleViewer}

func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleDeveloper:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// Valid tells if r is a known role.
func (r Role) Valid() bool { return r.rank() > 0 }

// AtLeast tells if r has the same or more permissions than min.
func (r Role) AtLeast(min Role) bool { return r.Valid() && r.rank() >= min.rank() }

// Name limits. They keep generated Kubernetes and Argo CD names shorter than
// 63 characters: "<tenant>-<environment>-<deployment>".
const (
	MaxTenantSlug     = 20
	MaxEnvironment    = 10
	MaxDeploymentName = 30
)

var (
	labelPattern       = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	envPattern         = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	quantityPattern    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|Ki|Mi|Gi|Ti)?$`)
	reservedTenantSlug = map[string]bool{
		"admin": true, "api": true, "argocd": true, "default": true, "forgejo": true,
		"harbor": true, "kube": true, "library": true, "platform": true, "squared": true, "system": true,
	}
)

// ValidateTenantSlug checks a tenant slug. Slugs are DNS labels because they
// become part of namespace and project names.
func ValidateTenantSlug(s string) error {
	switch {
	case len(s) < 3 || len(s) > MaxTenantSlug:
		return fmt.Errorf("must have 3 to %d characters", MaxTenantSlug)
	case !labelPattern.MatchString(s):
		return fmt.Errorf("must contain only lowercase letters, digits and '-', start with a letter and end with a letter or digit")
	case strings.Contains(s, "--"):
		return fmt.Errorf("must not contain '--'")
	case reservedTenantSlug[s] || strings.HasPrefix(s, "kube-"):
		return fmt.Errorf("is reserved")
	}
	return nil
}

// ValidateEnvironmentName checks an environment name. Environment names do
// not contain '-'. This makes "<tenant>-<environment>" unique.
func ValidateEnvironmentName(s string) error {
	if len(s) < 1 || len(s) > MaxEnvironment || !envPattern.MatchString(s) {
		return fmt.Errorf("must have 1 to %d lowercase letters or digits and start with a letter", MaxEnvironment)
	}
	return nil
}

// ValidateDeploymentName checks a deployment name.
func ValidateDeploymentName(s string) error {
	switch {
	case len(s) < 1 || len(s) > MaxDeploymentName:
		return fmt.Errorf("must have 1 to %d characters", MaxDeploymentName)
	case !labelPattern.MatchString(s):
		return fmt.Errorf("must contain only lowercase letters, digits and '-', start with a letter and end with a letter or digit")
	}
	return nil
}

// ValidateDisplayName checks a human readable name.
func ValidateDisplayName(s string) error {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 100 {
		return fmt.Errorf("must have 1 to 100 characters")
	}
	if strings.TrimSpace(s) != s {
		return fmt.Errorf("must not start or end with a space")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}

// NormalizeEmail returns the email address in a standard form, or an error
// if the address is not valid.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 254 {
		return "", fmt.Errorf("is too long")
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return "", fmt.Errorf("is not a valid email address")
	}
	return s, nil
}

// ValidatePassword checks the password rules. Long passwords are better
// than complex passwords, thus the only rule is a minimum length.
func ValidatePassword(p string) error {
	n := utf8.RuneCountInString(p)
	if n < 12 {
		return fmt.Errorf("must have at least 12 characters")
	}
	if len(p) > 256 {
		return fmt.Errorf("must have at most 256 bytes")
	}
	return nil
}

// ValidateQuota checks the quota of a tenant.
func ValidateQuota(q Quota) error {
	var v ValidationError
	if !quantityPattern.MatchString(q.CPU) {
		v.Add("quota_cpu", "must be a Kubernetes quantity, for example 2 or 500m")
	}
	if !quantityPattern.MatchString(q.Memory) {
		v.Add("quota_memory", "must be a Kubernetes quantity, for example 4Gi")
	}
	if q.Pods < 1 || q.Pods > 1000 {
		v.Add("quota_pods", "must be from 1 to 1000")
	}
	return v.Err()
}

// ValidQuantity tells if s is a Kubernetes resource quantity in a form
// that Squared accepts.
func ValidQuantity(s string) bool { return quantityPattern.MatchString(s) }

// Namespace returns the Kubernetes namespace of an environment.
func Namespace(tenantSlug, env string) string { return tenantSlug + "-" + env }

// AppName returns the name of the Argo CD Application of a deployment.
func AppName(tenantSlug, env, deployment string) string {
	return tenantSlug + "-" + env + "-" + deployment
}
