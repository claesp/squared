package guard

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"squared/internal/forgejo"
)

// forgejoOAuth2ClientKeys are the keys of the section [oauth2_client] of
// Forgejo 15 (modules/setting/oauth2.go). Forgejo reads key names with
// the exact case.
var forgejoOAuth2ClientKeys = []string{
	"ACCOUNT_LINKING", "ENABLE_AUTO_REGISTRATION", "OPENID_CONNECT_SCOPES",
	"REGISTER_EMAIL_CONFIRM", "UPDATE_AVATAR", "USERNAME",
}

// forgejoAccountLinking returns the value of ACCOUNT_LINKING that Forgejo
// 15 uses: "login" if the key is missing, empty or not valid.
func forgejoAccountLinking(section map[string]string) string {
	switch v := section["ACCOUNT_LINKING"]; v {
	case "disabled", "login", "auto":
		return v
	}
	return "login"
}

// checkForgejoOAuth2Client checks the sign-in settings of Forgejo. With
// ACCOUNT_LINKING = auto, Forgejo gives a first sign-in the existing
// account with the same user name or email address, without a check of
// email_verified. Then a Keycloak user can get the account of another
// person, for example the local administrator. The guard checks the
// values that Forgejo uses now (Start) and the values that it uses after
// the next start (Now). ignore are new keys that an operator checked.
func checkForgejoOAuth2Client(rep forgejo.SectionReport, ignore []string) ([]string, error) {
	var warnings, problems []string
	if rep.Error != "" {
		problems = append(problems, "the reporter cannot read app.ini: "+rep.Error)
	}
	for _, s := range []struct {
		when   string
		values map[string]string
	}{{"now", rep.Start}, {"after the next start", rep.Now}} {
		if s.values == nil {
			continue
		}
		switch forgejoAccountLinking(s.values) {
		case "auto":
			problems = append(problems, fmt.Sprintf("ACCOUNT_LINKING is auto %s: Forgejo gives a sign-in the existing account with the same user name or email address", s.when))
		case "login":
			if s.when == "now" {
				warnings = append(warnings, "Forgejo ACCOUNT_LINKING is login: a person who knows the password of a local account can link it to a Keycloak user; set it to disabled")
			}
		}
	}
	if rep.Now != nil && !maps.Equal(rep.Start, rep.Now) {
		problems = append(problems, "the section [oauth2_client] of app.ini changed after the start of Forgejo; Forgejo uses the old values until its next start")
	}
	keys := slices.Concat(slices.Collect(maps.Keys(rep.Start)), slices.Collect(maps.Keys(rep.Now)))
	slices.Sort(keys)
	for _, k := range slices.Compact(keys) {
		if slices.Contains(forgejoOAuth2ClientKeys, k) || slices.Contains(ignore, k) {
			continue
		}
		problems = append(problems, fmt.Sprintf("unknown key %s (a new or renamed setting, or a key that Forgejo does not read because of its case)", k))
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("the sign-in settings of Forgejo are not safe: %s. The guard keeps the current protection",
			strings.Join(problems, "; "))
	}
	return warnings, nil
}
