package guard

import (
	"fmt"
	"strconv"
	"strings"

	"squared/internal/kube"
)

// The SCM parameters of the ApplicationSet controller. The SCM provider
// and pull request generators send a token from a Secret in the
// namespace of the ApplicationSet to the API of a Git server.
var (
	scmEnabled = argoParam{"applicationsetcontroller.enable.scm.providers",
		"ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS", "--enable-scm-providers"}
	scmAllowed = argoParam{"applicationsetcontroller.allowed.scm.providers",
		"ARGOCD_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS", "--allowed-scm-providers"}
	scmStrict = argoParam{"applicationsetcontroller.enable.tokenref.strict.mode",
		"ARGOCD_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE", "--token-ref-strict-mode"}
)

// argoAppSetController is the workload of the ApplicationSet controller.
const argoAppSetController = "argocd-applicationset-controller"

// effectiveParam returns the value of a parameter for a container: the
// flag, else the environment variable, else the default (def). A
// variable from the key of the parameter in argocd-cmd-params-cm has the
// value there, and the default when the key is not there. isBool tells
// that the flag can have no value. It returns a problem for a variable
// from another place.
func effectiveParam(params map[string]string, c kube.Container, p argoParam, isBool bool, def string) (string, string) {
	args := append(append([]string{}, c.Command...), c.Args...)
	for i := len(args) - 1; i >= 0; i-- {
		a := args[i]
		if v, ok := strings.CutPrefix(a, p.Flag+"="); ok {
			return v, ""
		}
		if a == p.Flag {
			if isBool {
				return "true", ""
			}
			if i+1 < len(args) {
				return args[i+1], ""
			}
		}
	}
	for _, e := range c.Env {
		if e.Name != p.Env {
			continue
		}
		switch {
		case e.ValueFrom == nil:
			return e.Value, ""
		case e.ValueFrom.ConfigMapKeyRef != nil &&
			e.ValueFrom.ConfigMapKeyRef.Name == argoParamsConfigMap && e.ValueFrom.ConfigMapKeyRef.Key == p.Key:
			if v, ok := params[p.Key]; ok {
				return v, ""
			}
			return def, ""
		default:
			return def, fmt.Sprintf("container %s reads %s from another place than the key %s of %s", c.Name, p.Env, p.Key, argoParamsConfigMap)
		}
	}
	return def, ""
}

// checkArgoSCM returns the problems of the SCM parameters of the
// containers of the ApplicationSet controller. Without the controller,
// no generator runs. Squared needs no SCM generators, thus off is the
// safe value. When they are on, the guard requires a list of API
// addresses with TLS, and the strict mode for the token Secrets: else an
// ApplicationSet can send each Secret of its namespace, for example the
// Forgejo credentials of Argo CD, to each server.
func checkArgoSCM(params map[string]string, containers []kube.Container) []string {
	var p []string
	for _, c := range containers {
		if len(c.EnvFrom) > 0 {
			p = append(p, fmt.Sprintf("container %s reads environment variables with envFrom, which the guard does not read", c.Name))
			continue
		}
		enabledValue, problem := effectiveParam(params, c, scmEnabled, true, "true")
		if problem != "" {
			p = append(p, problem)
			continue
		}
		enabled, err := strconv.ParseBool(strings.TrimSpace(enabledValue))
		if err != nil {
			p = append(p, fmt.Sprintf("container %s has the value %q for %s, which is not true or false", c.Name, enabledValue, scmEnabled.Key))
			continue
		}
		if !enabled {
			continue
		}
		on := fmt.Sprintf("the SCM generators are on in container %s (%s)", c.Name, scmEnabled.Key)

		allowedValue, problem := effectiveParam(params, c, scmAllowed, false, "")
		if problem != "" {
			p = append(p, problem)
		}
		var allowed []string
		for _, a := range strings.Split(allowedValue, ",") {
			if a = strings.TrimSpace(a); a != "" {
				allowed = append(allowed, a)
			}
		}
		if len(allowed) == 0 {
			p = append(p, on+fmt.Sprintf(" and %s is empty: an ApplicationSet can send tokens to each server, and Squared needs no SCM generators", scmAllowed.Key))
		}
		for _, a := range allowed {
			if !strings.HasPrefix(strings.ToLower(a), "https://") {
				p = append(p, on+fmt.Sprintf(" and %s has %s without TLS: tokens go as plain text", scmAllowed.Key, a))
			}
		}

		strictValue, problem := effectiveParam(params, c, scmStrict, true, "false")
		if problem != "" {
			p = append(p, problem)
			continue
		}
		if strict, err := strconv.ParseBool(strings.TrimSpace(strictValue)); err != nil || !strict {
			p = append(p, on+fmt.Sprintf(" and %s is not true: an ApplicationSet can use each Secret of its namespace as token, also the repository credentials of Argo CD", scmStrict.Key))
		}
	}
	return p
}
