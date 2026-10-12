package guard

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"squared/internal/kube"
)

// The policy parameters of the ApplicationSet controller. The policy tells
// what an ApplicationSet can do with the applications that it made:
// "sync" (create, update and delete), "create-only", "create-update" or
// "create-delete". Without a policy, ApplicationSets use "sync" and can
// choose another one. With a policy, they can choose another one only
// when the override is on (the default).
var (
	setPolicy = argoParam{"applicationsetcontroller.policy",
		"ARGOCD_APPLICATIONSET_CONTROLLER_POLICY", "--policy"}
	setPolicyOverride = argoParam{"applicationsetcontroller.enable.policy.override",
		"ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE", "--enable-policy-override"}
)

var appSetPolicies = []string{"sync", "create-only", "create-update", "create-delete"}

// policyUpdates tells if a policy lets an ApplicationSet update the
// applications that it made.
func policyUpdates(p string) bool { return p == "sync" || p == "create-update" }

// argoSetPolicy is the policy of the ApplicationSet controller.
type argoSetPolicy struct {
	Running  bool   // The controller exists.
	Policy   string // The policy of the controller, or "".
	Override bool   // ApplicationSets can choose another policy.
}

// effective returns the policy that an ApplicationSet with its own policy
// (own, or "") gets.
func (c argoSetPolicy) effective(own string) string {
	switch {
	case c.Policy == "" || c.Override:
		return cmp.Or(own, c.Policy, "sync")
	default:
		return c.Policy
	}
}

// checkArgoSetPolicy returns the policy of the containers of the
// ApplicationSet controller, and its problems. Squared needs no
// ApplicationSets. An ApplicationSet that takes over an application of
// Squared (see checkArgoAppSets) writes its template into it. With a
// policy that permits updates, it writes it again after each repair by
// the root application, and Argo CD can each time remove the objects of
// the tenant that are not in the template (prune). Thus the policy must
// be create-only or create-delete. The containers with envFrom are
// problems of checkArgoSCM.
func checkArgoSetPolicy(params map[string]string, containers []kube.Container) (argoSetPolicy, []string) {
	var c argoSetPolicy
	var p []string
	for _, ct := range containers {
		if len(ct.EnvFrom) > 0 {
			continue
		}
		c.Running = true
		policy, problem := effectiveParam(params, ct, setPolicy, false, "")
		if problem != "" {
			p = append(p, problem)
		}
		policy = strings.TrimSpace(policy)
		switch {
		case policy != "" && !slices.Contains(appSetPolicies, policy):
			p = append(p, fmt.Sprintf("container %s has the value %q for %s, which Argo CD does not know", ct.Name, policy, setPolicy.Key))
		case !policyUpdates(cmp.Or(policy, "sync")):
		case policy == "":
			p = append(p, fmt.Sprintf("container %s has no %s, thus ApplicationSets use the policy sync, which permits updates of the applications, and Squared needs create-only", ct.Name, setPolicy.Key))
		default:
			p = append(p, fmt.Sprintf("container %s has the policy %s in %s, which permits updates of the applications, and Squared needs create-only", ct.Name, policy, setPolicy.Key))
		}
		if c.Policy == "" || policyUpdates(policy) {
			c.Policy = policy
		}

		override, problem := effectiveParam(params, ct, setPolicyOverride, true, "true")
		if problem != "" {
			p = append(p, problem)
			c.Override = true
			continue
		}
		on, err := strconv.ParseBool(strings.TrimSpace(override))
		if err != nil {
			p = append(p, fmt.Sprintf("container %s has the value %q for %s, which is not true or false", ct.Name, override, setPolicyOverride.Key))
			on = true
		}
		c.Override = c.Override || on
	}
	return c, p
}

// appSetPolicyProblems returns the problems of the policy of one
// ApplicationSet (own). A policy of the controller that permits updates
// is a problem of the controller only.
func appSetPolicyProblems(c argoSetPolicy, own string) []string {
	if !c.Running {
		return nil
	}
	if own != "" && !slices.Contains(appSetPolicies, own) {
		return []string{fmt.Sprintf("Argo CD does not know its policy %q", own)}
	}
	if policyUpdates(cmp.Or(c.Policy, "sync")) {
		return nil
	}
	if e := c.effective(own); policyUpdates(e) {
		return []string{fmt.Sprintf("its policy %s permits updates of its applications, because %s is on", e, setPolicyOverride.Key)}
	}
	return nil
}
