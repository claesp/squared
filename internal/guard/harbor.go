package guard

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// harborSettings are the configuration keys of Harbor 2.15
// (src/lib/config/metadata/metadatalist.go). Harbor keeps only the keys
// that somebody set, thus a missing key tells nothing. A key that the
// guard does not know tells that an upgrade added or renamed a setting.
var harborSettings = func() map[string]fieldKind {
	m := map[string]fieldKind{
		"oidc_admin_group":            fieldRights, // Members are system administrators.
		"http_authproxy_admin_groups": fieldRights, // The same for the authentication proxy.
		"auth_mode":                   fieldRelated,
		"primary_auth_mode":           fieldRelated,
		"oidc_groups_claim":           fieldRelated,
		"oidc_group_filter":           fieldRelated,
		// An LDAP group gives rights only with LDAP sign-in. It is a DN, not a
		// Keycloak group.
		"ldap_group_admin_dn": fieldOther,
	}
	for _, k := range harborOtherSettings {
		m[k] = fieldOther
	}
	return m
}()

// harborOtherSettings have nothing to do with the groups of the identity
// provider.
var harborOtherSettings = []string{
	"REPLICATION_ADAPTER_WHITELIST", "admin_initial_password", "audit_log_forward_endpoint",
	"banner_message", "beego_max_memory_bytes", "beego_max_upload_size_bytes", "cache_enabled",
	"cache_expire_hours", "core_local_url", "core_url", "database_type", "disabled_audit_log_event_types",
	"execution_status_refresh_interval_seconds", "ext_endpoint", "gdpr_audit_logs", "gdpr_delete_user",
	"http_authproxy_admin_usernames", "http_authproxy_endpoint", "http_authproxy_server_certificate",
	"http_authproxy_skip_search", "http_authproxy_tokenreview_endpoint", "http_authproxy_verify_cert",
	"jobservice_url", "ldap_base_dn", "ldap_filter", "ldap_group_attach_parallel", "ldap_group_attribute_name",
	"ldap_group_base_dn", "ldap_group_membership_attribute", "ldap_group_search_filter",
	"ldap_group_search_scope", "ldap_scope", "ldap_search_dn", "ldap_search_password", "ldap_timeout",
	"ldap_uid", "ldap_url", "ldap_verify_cert", "max_job_workers", "metric_enable", "metric_path",
	"metric_port", "notification_enable", "oidc_auto_onboard", "oidc_client_id", "oidc_client_secret",
	"oidc_endpoint", "oidc_extra_redirect_parms", "oidc_logout", "oidc_name", "oidc_scope",
	"oidc_user_claim", "oidc_verify_cert", "postgresql_conn_max_idle_time", "postgresql_conn_max_lifetime",
	"postgresql_database", "postgresql_host", "postgresql_max_idle_conns", "postgresql_max_open_conns",
	"postgresql_password", "postgresql_port", "postgresql_sslmode", "postgresql_username",
	"project_creation_restriction", "pull_audit_log_disable", "pull_count_update_disable",
	"pull_time_update_disable", "quota_per_project_enable", "quota_update_provider", "read_only",
	"registry_controller_url", "registry_storage_provider_name", "registry_url", "robot_name_prefix",
	"robot_scanner_name_prefix", "robot_token_duration", "scan_all_policy", "scanner_skip_update_pulltime",
	"self_registration", "session_timeout", "skip_audit_log_database", "storage_per_project",
	"token_expiration", "token_service_url", "trace_attribute", "trace_enabled", "trace_jaeger_agent_host",
	"trace_jaeger_agent_port", "trace_jaeger_endpoint", "trace_jaeger_password", "trace_jaeger_username",
	"trace_namespace", "trace_otel_compression", "trace_otel_endpoint", "trace_otel_insecure",
	"trace_otel_timeout", "trace_otel_url_path", "trace_sample_rate", "trace_service_name",
	"trivy_adapter_url", "uaa_client_id", "uaa_client_secret", "uaa_endpoint", "uaa_verify_cert", "with_trivy",
}

// checkHarborConfig returns the groups that the Harbor configuration
// gives rights to. settings has all keys of the view
// squared_guard_harbor_config; the value of a key is empty if the view
// does not show it. It returns an error if a key can give rights but the
// guard does not know it, because the guard can then miss a group. ignore
// are unknown keys that an operator checked and accepted.
func checkHarborConfig(settings map[string]string, ignore []string) ([]Reference, []string, error) {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var warnings, problems []string
	for _, k := range keys {
		if _, ok := harborSettings[k]; ok || slices.Contains(ignore, k) {
			continue
		}
		if rightsLike.MatchString(k) {
			problems = append(problems, "new setting "+k+" can give rights")
		} else {
			warnings = append(warnings, fmt.Sprintf("Harbor has the new setting %s; check that it gives no rights to groups, then add it to ignoreFields", k))
		}
	}
	if len(problems) > 0 {
		return nil, warnings, fmt.Errorf("the settings of Harbor changed: %s. The guard cannot read the groups of Harbor; it keeps the current protection. Update the guard for this Harbor version",
			strings.Join(problems, "; "))
	}

	// The groups are protected in all authentication modes, because an
	// administrator can change the mode at any time.
	var refs []Reference
	if g := strings.TrimSpace(settings["oidc_admin_group"]); g != "" {
		refs = append(refs, Reference{Group: g, Where: "setting oidc_admin_group"})
	}
	for _, g := range splitGroups(settings["http_authproxy_admin_groups"]) {
		refs = append(refs, Reference{Group: g, Where: "setting http_authproxy_admin_groups"})
	}
	if mode := settings["auth_mode"]; mode != "" && mode != "oidc_auth" {
		warnings = append(warnings, fmt.Sprintf("Harbor uses the authentication mode %s, not oidc_auth", mode))
	}
	return refs, warnings, nil
}
