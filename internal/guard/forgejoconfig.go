package guard

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"squared/internal/forgejo"
)

// forgejoKnownKeys are the keys of the sign-in sections in Forgejo 15
// (modules/setting/admin.go, oauth2.go, service.go, mailer.go, webhook.go
// and migrations.go). Forgejo reads key
// names with the exact case.
var forgejoKnownKeys = map[string][]string{
	"admin": {"DEFAULT_EMAIL_NOTIFICATIONS", "DISABLE_REGULAR_ORG_CREATION", "EXTERNAL_USER_DISABLE_FEATURES",
		"SEND_NOTIFICATION_EMAIL_ON_NEW_USER", "USER_DISABLED_FEATURES"},
	"oauth2_client": {"ACCOUNT_LINKING", "ENABLE_AUTO_REGISTRATION", "OPENID_CONNECT_SCOPES",
		"REGISTER_EMAIL_CONFIRM", "UPDATE_AVATAR", "USERNAME"},
	"openid": {"BLACKLISTED_URIS", "ENABLE_OPENID_SIGNIN", "ENABLE_OPENID_SIGNUP", "WHITELISTED_URIS"},
	"service": {"ACTIVE_CODE_LIVE_MINUTES", "ALLOW_CROSS_REPOSITORY_DEPENDENCIES", "ALLOW_DOTS_IN_USERNAMES",
		"ALLOWED_USER_VISIBILITY_MODES", "ALLOW_ONLY_EXTERNAL_REGISTRATION", "ALLOW_ONLY_INTERNAL_REGISTRATION",
		"AUTO_WATCH_NEW_REPOS", "AUTO_WATCH_ON_CHANGES", "CAPTCHA_TYPE", "CF_TURNSTILE_SECRET", "CF_TURNSTILE_SITEKEY",
		"DEFAULT_ALLOW_CREATE_ORGANIZATION", "DEFAULT_ALLOW_ONLY_CONTRIBUTORS_TO_TRACK_TIME", "DEFAULT_ENABLE_DEPENDENCIES",
		"DEFAULT_ENABLE_TIMETRACKING", "DEFAULT_KEEP_EMAIL_PRIVATE", "DEFAULT_ORG_MEMBER_VISIBLE", "DEFAULT_ORG_VISIBILITY",
		"DEFAULT_USER_IS_RESTRICTED", "DEFAULT_USER_VISIBILITY", "DISABLE_REGISTRATION", "EMAIL_DOMAIN_ALLOWLIST",
		"EMAIL_DOMAIN_BLOCK_DISPOSABLE", "EMAIL_DOMAIN_BLOCKLIST", "EMAIL_DOMAIN_WHITELIST", "ENABLE_BASIC_AUTHENTICATION",
		"ENABLE_CAPTCHA", "ENABLE_INTERNAL_SIGNIN", "ENABLE_NOTIFY_MAIL", "ENABLE_REVERSE_PROXY_AUTHENTICATION",
		"ENABLE_REVERSE_PROXY_AUTHENTICATION_API", "ENABLE_REVERSE_PROXY_AUTO_REGISTRATION", "ENABLE_REVERSE_PROXY_EMAIL",
		"ENABLE_REVERSE_PROXY_FULL_NAME", "ENABLE_TIMETRACKING", "ENABLE_USER_HEATMAP", "HCAPTCHA_SECRET", "HCAPTCHA_SITEKEY",
		"MAX_USER_REDIRECTS", "MCAPTCHA_SECRET", "MCAPTCHA_SITEKEY", "MCAPTCHA_URL", "NO_REPLY_ADDRESS", "RECAPTCHA_SECRET",
		"RECAPTCHA_SITEKEY", "RECAPTCHA_URL", "REGISTER_EMAIL_CONFIRM", "REGISTER_MANUAL_CONFIRM", "REQUIRE_CAPTCHA_FOR_LOGIN",
		"REQUIRE_EXTERNAL_REGISTRATION_CAPTCHA", "REQUIRE_EXTERNAL_REGISTRATION_PASSWORD", "REQUIRE_SIGNIN_VIEW",
		"RESET_PASSWD_CODE_LIVE_MINUTES", "SHOW_MILESTONES_DASHBOARD_PAGE", "SHOW_REGISTRATION_BUTTON",
		"USER_DELETE_WITH_COMMENTS_MAX_TIME", "USER_LOCATION_MAP_URL", "USERNAME_COOLDOWN_PERIOD", "VALID_SITE_URL_SCHEMES"},
	"security":       {"IMPORT_LOCAL_PATHS", "INSTALL_LOCK"},
	"mailer":         {"FORCE_TRUST_SERVER_CERT", "SKIP_VERIFY"}, // The reporter serves only these keys.
	"email.incoming": {"SKIP_TLS_VERIFY"},                        // The reporter serves only this key.
	"migrations":     {"ALLOWED_DOMAINS", "ALLOW_LOCALNETWORKS", "BLOCKED_DOMAINS", "MAX_ATTEMPTS", "RETRY_BACKOFF", "SKIP_TLS_VERIFY"},
	"webhook": {"ALLOWED_HOST_LIST", "DELIVER_TIMEOUT", "PAGING_NUM", "PAYLOAD_COMMIT_LIMIT", "PROXY_HOSTS", "PROXY_URL",
		"QUEUE_LENGTH", "SKIP_TLS_VERIFY"},
}

// signInLike matches the names of keys that can change who can sign in or
// make an account. An unknown key in [service] or [admin] with such a name
// is an error; other unknown keys of these sections are warnings. All
// unknown keys of [oauth2_client], [openid], [webhook] and [migrations] are
// errors.
var signInLike = regexp.MustCompile(`(?i)regist|sign_?up|sign_?in|login|auth|link|external|internal|proxy|openid|account|passw|featur`)

// iniBool reads a boolean as go-ini does for Forgejo (MustBool): an
// empty or not valid value gives def.
func iniBool(v string, def bool) bool {
	switch v {
	case "1", "t", "T", "true", "TRUE", "True", "YES", "yes", "Yes", "y", "ON", "on", "On":
		return true
	case "0", "f", "F", "false", "FALSE", "False", "NO", "no", "No", "n", "OFF", "off", "Off":
		return false
	}
	return def
}

// forgejoSignIn are the sign-in and registration settings that Forgejo 15
// uses, with its defaults.
type forgejoSignIn struct {
	accountLinking      string // "disabled", "login" or "auto"
	autoRegistration    bool   // oauth2_client ENABLE_AUTO_REGISTRATION
	disableRegistration bool
	onlyInternal        bool // ALLOW_ONLY_INTERNAL_REGISTRATION
	onlyExternal        bool // ALLOW_ONLY_EXTERNAL_REGISTRATION
	openIDSignIn        bool
	openIDSignUp        bool
	reverseProxyAuth    bool // ENABLE_REVERSE_PROXY_AUTHENTICATION or _API
	reverseProxyNew     bool // ENABLE_REVERSE_PROXY_AUTO_REGISTRATION
	internalSignIn      bool // ENABLE_INTERNAL_SIGNIN: the web form accepts passwords.
	// webhookHosts are the entries of ALLOWED_HOST_LIST, in lower case;
	// "external" without a value, as in Forgejo.
	webhookHosts []string
	// importLocalPaths is IMPORT_LOCAL_PATHS of [security]: administrators
	// and users with the flag allow_import_local can import directories of
	// the server.
	importLocalPaths bool
	// allowLocalNetworks is ALLOW_LOCALNETWORKS of [migrations]: migrations
	// and mirrors can fetch from private and loopback addresses.
	allowLocalNetworks bool
	// migrationDomains are the entries of ALLOWED_DOMAINS of [migrations],
	// in lower case.
	migrationDomains []string
	// tlsUnchecked are the settings that turn off the check of TLS
	// certificates, as "<key> of [<section>]: <what goes without the check>".
	tlsUnchecked []string
	// externalPasswords tells that Keycloak users can set a Forgejo
	// password: "manage_password" is not in USER_DISABLED_FEATURES or
	// EXTERNAL_USER_DISABLE_FEATURES.
	externalPasswords bool
}

func forgejoSignInFrom(s map[string]map[string]string) forgejoSignIn {
	var f forgejoSignIn
	oc, svc, oid := s["oauth2_client"], s["service"], s["openid"]
	switch v := oc["ACCOUNT_LINKING"]; v {
	case "disabled", "login", "auto":
		f.accountLinking = v
	default:
		f.accountLinking = "login" // Forgejo uses "login" for a missing or not valid value.
	}
	f.autoRegistration = iniBool(oc["ENABLE_AUTO_REGISTRATION"], false)
	f.disableRegistration = iniBool(svc["DISABLE_REGISTRATION"], false)
	f.onlyInternal = iniBool(svc["ALLOW_ONLY_INTERNAL_REGISTRATION"], false)
	f.onlyExternal = iniBool(svc["ALLOW_ONLY_EXTERNAL_REGISTRATION"], false)
	if f.onlyInternal && f.onlyExternal {
		f.disableRegistration = true // As Forgejo does.
	}
	installLock := iniBool(s["security"]["INSTALL_LOCK"], false)
	f.openIDSignIn = iniBool(oid["ENABLE_OPENID_SIGNIN"], !installLock)
	f.openIDSignUp = iniBool(oid["ENABLE_OPENID_SIGNUP"], !f.disableRegistration && f.openIDSignIn)
	f.reverseProxyAuth = iniBool(svc["ENABLE_REVERSE_PROXY_AUTHENTICATION"], false) ||
		iniBool(svc["ENABLE_REVERSE_PROXY_AUTHENTICATION_API"], false)
	f.reverseProxyNew = iniBool(svc["ENABLE_REVERSE_PROXY_AUTO_REGISTRATION"], false)
	f.internalSignIn = iniBool(svc["ENABLE_INTERNAL_SIGNIN"], true)
	f.webhookHosts = nil
	for _, e := range iniList(s["webhook"]["ALLOWED_HOST_LIST"]) {
		f.webhookHosts = append(f.webhookHosts, strings.ToLower(e))
	}
	if len(f.webhookHosts) == 0 {
		f.webhookHosts = []string{"external"}
	}
	f.importLocalPaths = iniBool(s["security"]["IMPORT_LOCAL_PATHS"], false)
	f.tlsUnchecked = tlsUnchecked(s)
	mig := s["migrations"]
	f.allowLocalNetworks = iniBool(mig["ALLOW_LOCALNETWORKS"], false)
	f.migrationDomains = nil
	for _, e := range iniList(mig["ALLOWED_DOMAINS"]) {
		f.migrationDomains = append(f.migrationDomains, strings.ToLower(e))
	}
	adm := s["admin"]
	f.externalPasswords = !slices.Contains(iniList(adm["USER_DISABLED_FEATURES"]), "manage_password") &&
		!slices.Contains(iniList(adm["EXTERNAL_USER_DISABLE_FEATURES"]), "manage_password")
	return f
}

// iniList reads a list with commas as go-ini does for Forgejo (Strings).
func iniList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// problems returns the settings that let a person get an account or the
// account of another person without Keycloak, and the hosts that webhooks
// can send requests to although allowedHosts does not have them. A
// message has no ";": the guard joins the messages with it.
func (f forgejoSignIn) problems(allowedHosts []string) []string {
	var p []string
	var hosts []string
	for _, h := range f.webhookHosts {
		if h != "external" && !slices.ContainsFunc(allowedHosts, func(a string) bool { return strings.EqualFold(a, h) }) {
			hosts = append(hosts, h)
		}
	}
	for _, t := range f.tlsUnchecked {
		p = append(p, t+", thus a person in the network between Forgejo and the server can read or change the data")
	}
	if f.importLocalPaths {
		p = append(p, "IMPORT_LOCAL_PATHS of [security] is on: Forgejo administrators and users with the flag allow_import_local can copy any directory of the Forgejo server into a repository, for example the repositories of other users")
	}
	if f.allowLocalNetworks {
		p = append(p, "ALLOW_LOCALNETWORKS of [migrations] is on: users can make Forgejo fetch repositories (migrations and mirrors) from private and loopback addresses, for example the services in the cluster")
	}
	for _, d := range f.migrationDomains {
		if probe := clusterNameFor(d); probe != "" {
			p = append(p, fmt.Sprintf("ALLOWED_DOMAINS of [migrations] has %s, which matches the name %s in the cluster: users can make Forgejo fetch repositories from the services in the cluster", d, probe))
		}
	}
	if len(hosts) > 0 {
		p = append(p, fmt.Sprintf("ALLOWED_HOST_LIST of [webhook] has %s: the webhooks of all Forgejo users, also of personal repositories, can send requests there, and a keyword like private or loopback, a network, or a host pattern reaches the services in the cluster. Permit only external and the hosts in webhookHosts",
			strings.Join(hosts, ", ")))
	}
	if f.accountLinking == "auto" {
		p = append(p, "ACCOUNT_LINKING is auto: Forgejo gives a sign-in the existing account with the same user name or email address")
	}
	if !f.disableRegistration && !f.onlyExternal {
		p = append(p, "local registration is open (DISABLE_REGISTRATION and ALLOW_ONLY_EXTERNAL_REGISTRATION are false): anybody can make an account without Keycloak")
	}
	if f.openIDSignIn {
		p = append(p, "ENABLE_OPENID_SIGNIN is on: people can sign in with OpenID 2.0 addresses that they add to their accounts, without Keycloak, also after Keycloak disabled them")
	}
	if f.openIDSignIn && f.openIDSignUp {
		p = append(p, "ENABLE_OPENID_SIGNUP is on: anybody with an OpenID 2.0 provider can make an account")
	}
	if f.internalSignIn {
		p = append(p, "ENABLE_INTERNAL_SIGNIN is on: the web sign-in form accepts passwords, thus local accounts, and Keycloak users with a Forgejo password, sign in without Keycloak")
	}
	if f.externalPasswords {
		p = append(p, "Keycloak users can set a Forgejo password (manage_password is not in EXTERNAL_USER_DISABLE_FEATURES of [admin]): Forgejo accepts it for API basic authentication, and for the web form if ENABLE_INTERNAL_SIGNIN is on, also after Keycloak disabled the user")
	}
	if f.reverseProxyAuth {
		p = append(p, "reverse proxy authentication is on: Forgejo accepts the user name in a header from every client that can connect to it")
		if f.reverseProxyNew {
			p = append(p, "ENABLE_REVERSE_PROXY_AUTO_REGISTRATION is on: such a header also makes new accounts")
		}
	}
	return p
}

// warnings returns the settings that are safe, but not as strict as they
// can be.
func (f forgejoSignIn) warnings() []string {
	var w []string
	if f.accountLinking == "login" {
		w = append(w, "Forgejo ACCOUNT_LINKING is login: a person who knows the password of a local account can link it to a Keycloak user; set it to disabled")
	}
	if f.onlyInternal || !f.autoRegistration {
		w = append(w, "Forgejo does not make accounts for Keycloak users (ENABLE_AUTO_REGISTRATION is off or ALLOW_ONLY_INTERNAL_REGISTRATION is on): a Keycloak user without an account gets a page to link a local account with its password")
	}
	return w
}

// checkForgejoSettings checks the sign-in, registration and webhook
// settings of Forgejo. Forgejo accounts must come only from Keycloak, only
// Forgejo must find the account of a sign-in, by the Keycloak user ID, and
// webhooks must not reach the services in the cluster other than
// webhookHosts. The guard checks the values that Forgejo uses now (Start)
// and the values that it uses after the next start (Now). ignore are new
// keys that an operator checked, as "<section>.<key>".
func checkForgejoSettings(rep forgejo.ConfigReport, ignore, webhookHosts []string) ([]string, error) {
	var problems []string
	if rep.Error != "" {
		problems = append(problems, "the reporter cannot read app.ini: "+rep.Error)
	}
	var warnings []string
	if rep.Start != nil {
		now := forgejoSignInFrom(rep.Start)
		problems = append(problems, now.problems(webhookHosts)...)
		warnings = now.warnings()
	}
	if rep.Now != nil {
		for _, p := range forgejoSignInFrom(rep.Now).problems(webhookHosts) {
			if !slices.Contains(problems, p) {
				problems = append(problems, p+" (after the next start)")
			}
		}
	}
	sections := slices.Concat(slices.Collect(maps.Keys(rep.Start)), slices.Collect(maps.Keys(rep.Now)))
	slices.Sort(sections)
	for _, section := range slices.Compact(sections) {
		if rep.Now != nil && !maps.Equal(rep.Start[section], rep.Now[section]) {
			problems = append(problems, fmt.Sprintf("the section [%s] of app.ini changed after the start of Forgejo, which uses the old values until its next start", section))
		}
		if _, known := forgejoKnownKeys[section]; !known {
			continue // A storage section, with only MINIO_INSECURE_SKIP_VERIFY.
		}
		keys := slices.Concat(slices.Collect(maps.Keys(rep.Start[section])), slices.Collect(maps.Keys(rep.Now[section])))
		slices.Sort(keys)
		for _, k := range slices.Compact(keys) {
			name := section + "." + k
			if slices.Contains(forgejoKnownKeys[section], k) || slices.Contains(ignore, name) {
				continue
			}
			msg := fmt.Sprintf("unknown key %s (a new or renamed setting, or a key that Forgejo does not read because of its case)", name)
			if (section == "service" || section == "admin") && !signInLike.MatchString(k) {
				warnings = append(warnings, "Forgejo app.ini has the "+msg+"; check it, then add it to ignoreFields")
			} else {
				problems = append(problems, msg)
			}
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the settings of Forgejo are not safe: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}

// clusterProbes are names that reach services in the cluster or on the
// node. With the DNS search domains of Kubernetes, a short name like
// "service" or "service.namespace" also reaches a service.
var clusterProbes = []string{
	"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local",
	"localhost", "node.local", "metadata.internal",
}

// clusterNameFor returns a probe name that the pattern of ALLOWED_DOMAINS
// matches, or "". Forgejo matches the patterns as file name patterns
// (path.Match), in lower case.
func clusterNameFor(pattern string) string {
	for _, probe := range clusterProbes {
		if ok, _ := path.Match(pattern, probe); ok {
			return probe
		}
	}
	return ""
}

// tlsUnchecked returns the settings that turn off the check of TLS
// certificates, with the defaults of Forgejo 15 (all off).
func tlsUnchecked(s map[string]map[string]string) []string {
	var out []string
	add := func(on bool, key, section, what string) {
		if on {
			out = append(out, fmt.Sprintf("%s of [%s] is on: Forgejo does not check the certificate of %s", key, section, what))
		}
	}
	add(iniBool(s["webhook"]["SKIP_TLS_VERIFY"], false), "SKIP_TLS_VERIFY", "webhook", "the webhook receivers")
	add(iniBool(s["migrations"]["SKIP_TLS_VERIFY"], false), "SKIP_TLS_VERIFY", "migrations",
		"the sources of migrations and mirrors, whose content goes into repositories")
	add(iniBool(s["email.incoming"]["SKIP_TLS_VERIFY"], false), "SKIP_TLS_VERIFY", "email.incoming", "the IMAP server")
	// SKIP_VERIFY is the old name of FORCE_TRUST_SERVER_CERT; Forgejo reads
	// it only without the new key.
	mailer := s["mailer"]
	if v, ok := mailer["FORCE_TRUST_SERVER_CERT"]; ok {
		add(iniBool(v, false), "FORCE_TRUST_SERVER_CERT", "mailer", "the SMTP server, which gets the password and the mails")
	} else {
		add(iniBool(mailer["SKIP_VERIFY"], false), "SKIP_VERIFY", "mailer", "the SMTP server, which gets the password and the mails")
	}
	for _, section := range slices.Sorted(maps.Keys(s)) {
		add(iniBool(s[section]["MINIO_INSECURE_SKIP_VERIFY"], false), "MINIO_INSECURE_SKIP_VERIFY", section,
			"the object storage, which keeps files of repositories, LFS, attachments or packages")
	}
	return out
}
