#!/usr/bin/env bash
# End-to-end test against the Kind development environment.
#
# The test signs in to the web interface through the Keycloak login form,
# makes an API token, checks Keycloak access tokens for the API, checks
# single sign-on and administrator rights in Argo CD, Harbor and Forgejo,
# makes a tenant, pushes an image to Harbor, deploys the image, changes it, rolls
# it back and removes it. It also checks that pods of other namespaces
# cannot connect to the tenant. At the end it removes the tenant. Set
# KEEP_TENANT=1 to keep the tenant for inspection. Run hack/kind-up.sh
# first.
source "$(dirname "$0")/lib.sh"

need curl jq podman kubectl go

TENANT="e2e$(date +%s | tail -c 6)"
NS="${TENANT}-dev"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
ADMIN_USER="platform-admin"
ADMIN_EMAIL="platform-admin@squared.test"
ADMIN_PASSWORD="$(secret keycloak-admin-user-password)"
DEV_PASSWORD="$(secret keycloak-dev-user-password)"

step() { printf '\n\033[1;32m--- %s\033[0m\n' "$*" >&2; }

# api METHOD PATH [JSON] calls the Squared API and prints the body. It
# fails for status codes 400 and higher.
api() {
  local out status
  out="$(curl -sS -o "$WORK/body" -w '%{http_code}' -X "$1" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    "${SQUARED_URL}/api/v1$2" ${3:+-d "$3"})"
  status="$out"
  if (( status >= 400 )); then
    cat "$WORK/body" >&2
    die "$1 $2: status $status"
  fi
  cat "$WORK/body"
}

# wait_for DESCRIPTION SECONDS COMMAND... runs COMMAND until it succeeds.
wait_for() {
  local what="$1" timeout="$2" start
  shift 2
  start=$(date +%s)
  until "$@"; do
    (( $(date +%s) - start > timeout )) && die "timeout: $what"
    sleep 3
  done
  log "$what: done after $(( $(date +%s) - start ))s"
}

tenant_ready() { [[ "$(api GET "/tenants/$TENANT" | jq -r .status)" == "ready" ]]; }

deployment_is() {
  local want_status="$1" want_revision="$2" d
  d="$(api GET "/tenants/$TENANT/deployments/dev/hello")"
  local status revision
  status="$(jq -r .status <<<"$d")"
  revision="$(jq -r .revision <<<"$d")"
  if [[ "$status" == "failed" || "$status" == "degraded" ]]; then
    jq . <<<"$d" >&2
    die "deployment is $status"
  fi
  [[ "$status" == "$want_status" && "$revision" == "$want_revision" ]]
}

deployment_gone() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
    "${SQUARED_URL}/api/v1/tenants/$TENANT/deployments/dev/hello")"
  [[ "$code" == "404" ]]
}

# expect_greeting TEXT checks the "message" field of the hello service.
# The port-forward runs only during the check.
expect_greeting() {
  local got="" i pf
  k -n "$NS" port-forward svc/hello-web 18090:80 >/dev/null 2>&1 &
  pf=$!
  for i in $(seq 1 30); do
    got="$(curl -fsS --max-time 2 http://127.0.0.1:18090/ 2>/dev/null | jq -r .message 2>/dev/null)" && [[ "$got" == "$1" ]] && break
    sleep 1
  done
  kill "$pf" 2>/dev/null || true
  wait "$pf" 2>/dev/null || true
  [[ "$got" == "$1" ]] || die "greeting is '$got', expected '$1'"
  log "the service answers: $got"
}

# probe_phase prints the phase of the probe pod.
probe_phase() { k -n "${TENANT}-outside" get pod probe -o jsonpath='{.status.phase}' 2>/dev/null; }

probe_done() { [[ "$(probe_phase)" == "Succeeded" || "$(probe_phase)" == "Failed" ]]; }

# The helper hack/ssoclient signs in through the Keycloak form like a
# browser. Curl cannot do it, because it refuses the Secure cookies of
# Keycloak on keycloak.localhost.
SSO="$WORK/ssoclient"
(cd "$ROOT" && go build -o "$SSO" ./hack/ssoclient) || die "cannot build hack/ssoclient"

# kc_signin USER PASSWORD_FILE JAR signs in to Squared through Keycloak and
# writes the Squared session cookie to JAR for curl.
kc_signin() {
  local out
  out="$("$SSO" -user "$1" -password-file "$2" "login=${SQUARED_URL}/auth/login" "cookies=${SQUARED_URL}/,$3")" \
    || die "sign-in of $1 to Squared failed"
  [[ "$(head -1 <<<"$out" | jq -r .final)" == "${SQUARED_URL}/" ]] || die "sign-in of $1 ended at $(head -1 <<<"$out" | jq -r .final)"
}

# check_sso USER PASSWORD_FILE ADMIN signs in to Argo CD, Harbor and
# Forgejo in one browser session. It checks that only the first service
# shows the Keycloak form, and that the user is an administrator in each
# service if ADMIN is "true", and not an administrator if it is "false".
check_sso() {
  local user="$1" admin="$3" out got
  out="$("$SSO" -user "$user" -password-file "$2" \
    "login=http://localhost:30081/auth/login?return_url=http%3A%2F%2Flocalhost%3A30081%2Fapplications" \
    "get=http://localhost:30081/api/v1/session/userinfo" \
    "get=http://localhost:30081/api/v1/account/can-i/applications/sync/*/*" \
    "login=${HARBOR_URL}/c/oidc/login?redirect_url=%2Fharbor%2Fprojects" \
    "get=${HARBOR_URL}/api/v2.0/users/current" \
    "login=${FORGEJO_URL}/user/oauth2/keycloak" \
    "get=${FORGEJO_URL}/")" || die "single sign-on of $user failed"
  [[ "$(jq -s -c '[.[] | select(.login) | .form]' <<<"$out")" == "[true,false,false]" ]] \
    || die "single sign-on: Keycloak asked $user for the password more than one time"
  jq -s -e '.[1].body.loggedIn == true' <<<"$out" >/dev/null || die "Argo CD: $user is not signed in"
  got="$(jq -s -r '.[2].body.value' <<<"$out")"
  [[ "$got" == "$([[ "$admin" == true ]] && echo yes || echo no)" ]] || die "Argo CD: '$user can sync applications' is $got"
  # Harbor gives administrator rights from the group in admin_role_in_auth.
  got="$(jq -s -r '.[4].body | "\(.username) \(.sysadmin_flag or .admin_role_in_auth)"' <<<"$out")"
  [[ "$got" == "$user $admin" ]] || die "Harbor: user and administrator flag are '$got'"
  # Forgejo does not accept the browser session for its API. Check that
  # the session works on the dashboard, then read the account that the
  # sign-in made with the local administrator account.
  jq -s -e '.[6].status == 200 and (.[6].body | contains("Dashboard"))' <<<"$out" >/dev/null \
    || die "Forgejo: $user is not signed in"
  got="$(curl -fsS -u "forgejo-admin:$(secret forgejo-admin-password)" "${FORGEJO_URL}/api/v1/users/$user" | jq -r '"\(.login) \(.is_admin)"')"
  [[ "$got" == "$user $admin" ]] || die "Forgejo: user and administrator flag are '$got'"
  log "$user signs in to Argo CD, Harbor and Forgejo with one Keycloak session (administrator: $admin)"
}

# fj_admin PATH [CURL OPTIONS] calls the Forgejo API as the local
# administrator.
fj_admin() { curl -fsS -u "forgejo-admin:$(secret forgejo-admin-password)" "${FORGEJO_URL}/api/v1$1" "${@:2}"; }

# check_forgejo_linking signs in to Forgejo as two new Keycloak users who
# try to get existing Forgejo accounts:
#   - e2e-intruder has the address dev@e2e-intruder.test. Forgejo must not
#     give it the account "dev" (the first part of the address).
#   - forgejo-admin has the user name and the address of the local Forgejo
#     administrator. The sign-in must fail.
# Forgejo must find accounts only by the Keycloak user ID.
check_forgejo_linking() {
  local at kc_api="${KEYCLOAK_URL}/admin/realms/squared" pw="$WORK/intruder-password" source_id user email sub out got
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  openssl rand -base64 24 | tr -d '/+=\n' > "$pw"
  source_id="$(k -n forgejo exec deploy/forgejo -c forgejo -- gitea admin auth list | awk '$2 == "keycloak" {print $1}')"
  for user in e2e-intruder forgejo-admin; do
    email="dev@e2e-intruder.test"
    [[ "$user" == forgejo-admin ]] && email="$(fj_admin /users/forgejo-admin | jq -r .email)"
    curl -fsS -X POST -H "Authorization: Bearer $at" -H 'Content-Type: application/json' "$kc_api/users" \
      -d "$(jq -n --arg u "$user" --arg e "$email" --rawfile p "$pw" '{username: $u, email: $e, emailVerified: true, enabled: true,
        firstName: "E2e", lastName: "Intruder", credentials: [{type: "password", value: $p, temporary: false}]}')" >/dev/null
    sub="$(curl -fsS -H "Authorization: Bearer $at" "$kc_api/users?username=$user&exact=true" | jq -r '.[0].id')"
    out="$("$SSO" -user "$user" -password-file "$pw" "login=${FORGEJO_URL}/user/oauth2/keycloak" "get=${FORGEJO_URL}/")" \
      || die "the Keycloak sign-in of $user failed"
    got="$(fj_admin "/admin/users?source_id=$source_id&login_name=$sub" | jq -r '[.[].login] | join(",")')"
    if [[ "$user" == e2e-intruder ]]; then
      [[ "$got" == e2e-intruder ]] || die "Forgejo: the Keycloak user e2e-intruder has the account '$got', not its own"
      jq -s -e '.[1].body | contains("Dashboard")' <<<"$out" >/dev/null || die "Forgejo: e2e-intruder is not signed in"
      fj_admin /admin/users/e2e-intruder -X DELETE >/dev/null
    else
      [[ -z "$got" ]] || die "Forgejo: the Keycloak user forgejo-admin got the account '$got'"
      jq -s -e '.[1].body | contains("Dashboard") | not' <<<"$out" >/dev/null \
        || die "Forgejo: the Keycloak user forgejo-admin is signed in to an existing account"
    fi
    curl -fsS -X DELETE -H "Authorization: Bearer $at" "$kc_api/users/$sub" >/dev/null
  done
  [[ "$(fj_admin /users/dev | jq -r .email)" == dev@squared.test ]] || die "Forgejo: the account dev changed"
  log "Forgejo finds accounts by the Keycloak user ID, not by the user name or the email address"
}

# check_forgejo_passwords checks that nobody signs in to Forgejo with a
# password on the web: the sign-in page has only the Keycloak button, and
# a Keycloak user cannot set a Forgejo password.
check_forgejo_passwords() {
  local page out
  page="$(curl -fsS "${FORGEJO_URL}/user/login")"
  grep -q '/user/oauth2/keycloak' <<<"$page" || die "Forgejo: the sign-in page has no Keycloak button"
  ! grep -q 'id="user_name"' <<<"$page" || die "Forgejo: the sign-in page has a form for a password"
  out="$("$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
    "login=${FORGEJO_URL}/user/oauth2/keycloak" "get=${FORGEJO_URL}/user/settings/account")" || die "the sign-in of dev to Forgejo failed"
  jq -s -e '.[1].status == 200 and (.[1].body | contains("/user/settings/account/email"))' <<<"$out" >/dev/null \
    || die "Forgejo: dev cannot open the account settings"
  jq -s -e '.[1].body | contains("id=\"retype\"") | not' <<<"$out" >/dev/null || die "Forgejo: dev can set a Forgejo password"
  log "Forgejo accepts no passwords on the web, and Keycloak users cannot set one"
}

# check_forgejo_credentials makes a personal access token for dev, as dev
# can in the Forgejo settings. A token works without Keycloak, thus the
# guard must report it when Keycloak disables dev, and when dev is a Forgejo
# administrator without the Keycloak group. Then it does the same with an
# SSH key: a key also stays when SSH is off.
check_forgejo_credentials() {
  local at kc_api="${KEYCLOAK_URL}/admin/realms/squared" id user
  local since
  since="$(date +%s)"
  k -n forgejo exec deploy/forgejo -c forgejo -- gitea admin user generate-access-token \
    --username dev --token-name e2e-guard --scopes read:user --raw >/dev/null || die "cannot make a Forgejo token for dev"
  wait_for "a guard run without errors with the token of dev" 60 guard_clean_since "$since"
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  id="$(curl -fsS -H "Authorization: Bearer $at" "$kc_api/users?username=dev&exact=true" | jq -r '.[0].id')"
  user="$(curl -fsS -H "Authorization: Bearer $at" "$kc_api/users/$id")"
  guard_reports_keycloak "/users/$id" "$(jq '.enabled = false' <<<"$user")" "$user" \
    'Forgejo user dev has personal access tokens [^;]*but Keycloak disabled the user'

  fj_admin /admin/users/dev -X PATCH -H 'Content-Type: application/json' -d '{"admin": true}' >/dev/null
  guard_reports_admin() {
    guard_status | jq -e '[.errors[]? | select(test("Forgejo user dev is a Forgejo administrator, but not a member of /squared-admins"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the administrator flag of dev with a token" 60 guard_reports_admin
  fj_admin /admin/users/dev -X PATCH -H 'Content-Type: application/json' -d '{"admin": false}' >/dev/null
  wait_for "the guard has no errors without the administrator flag" 60 guard_clean

  k -n forgejo exec -i statefulset/forgejo-db -- psql -q -U forgejo -d forgejo -v ON_ERROR_STOP=1 \
    -c "DELETE FROM access_token WHERE name = 'e2e-guard'" >/dev/null
  log "the guard checks the personal access tokens of Forgejo against Keycloak"

  # A public key only; nobody has the private key.
  local key_id
  key_id="$(fj_admin /admin/users/dev/keys -X POST -H 'Content-Type: application/json' -d '{"title": "e2e-guard",
    "key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID4TDHFy+/o19ELvPPvxDdAIuFZEyHOBWar3sCd9pznf squared-e2e"}' | jq -r .id)"
  [[ "$key_id" =~ ^[0-9]+$ ]] || die "cannot add an SSH key for dev"
  guard_reports_keycloak "/users/$id" "$(jq '.enabled = false' <<<"$user")" "$user" \
    'Forgejo user dev has SSH keys [^;]*but Keycloak disabled the user'
  fj_admin "/admin/users/dev/keys/$key_id" -X DELETE >/dev/null
  log "the guard checks the SSH keys of Forgejo against Keycloak"
}

# kc_token USER PASSWORD prints a Keycloak access token for the API. It
# uses the password grant of the development client "squared-cli".
# dev_access prints the HTTP status of the Harbor project and of the
# Forgejo repository of the tenant for the user dev, after a new sign-in.
dev_access() {
  "$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
    "login=${HARBOR_URL}/c/oidc/login?redirect_url=%2Fharbor%2Fprojects" \
    "get=${HARBOR_URL}/api/v2.0/projects/$TENANT" \
    "login=${FORGEJO_URL}/user/oauth2/keycloak" \
    "get=${FORGEJO_URL}/squared/tenant-$TENANT" | jq -s -r '"\(.[1].status) \(.[3].status)"'
}

# dev_has_access ROLE tells if dev has the Harbor project role ROLE and can
# read the Forgejo repository.
dev_has_access() {
  local out
  out="$("$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
    "login=${HARBOR_URL}/c/oidc/login?redirect_url=%2Fharbor%2Fprojects" \
    "get=${HARBOR_URL}/api/v2.0/projects/$TENANT" \
    "login=${FORGEJO_URL}/user/oauth2/keycloak" \
    "get=${FORGEJO_URL}/squared/tenant-$TENANT")"
  jq -s -e --argjson role "$1" '.[1].body.current_user_role_id == $role and .[3].status == 200' <<<"$out" >/dev/null
}

# check_worker_limits checks that the service account of the worker can
# change only the memberships of groups that are not protected.
check_worker_limits() {
  local wt at a="${KEYCLOAK_URL}/admin/realms/squared" dev admins probe sub code
  wt="$(curl -fsS -u "squared-worker:$(secret keycloak-squared-worker-client-secret)" \
    "${KEYCLOAK_URL}/realms/squared/protocol/openid-connect/token" -d grant_type=client_credentials | jq -r .access_token)"
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  wk() { curl -sS -o "$WORK/kc.out" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $wt" -H 'Content-Type: application/json' "$a$2" ${3:+-d "$3"}; }
  dev="$(curl -fsS -H "Authorization: Bearer $at" "$a/users?username=dev&exact=true" | jq -r '.[0].id')"
  local padmin
  padmin="$(curl -fsS -H "Authorization: Bearer $at" "$a/users?username=$ADMIN_USER&exact=true" | jq -r '.[0].id')"
  admins="$(curl -fsS -H "Authorization: Bearer $at" "$a/groups?search=squared-admins&exact=true" | jq -r '.[0].id')"
  [[ "$(wk GET '/users?email=dev@squared.test&exact=true')" == 200 ]] || die "the worker cannot find users"
  for probe in "PUT|/users/$dev/groups/$admins|" "DELETE|/users/$padmin/groups/$admins|" "DELETE|/groups/$admins|" \
      "PUT|/groups/$admins|{\"name\":\"renamed\"}" "POST|/groups/$admins/children|{\"name\":\"x\"}" "POST|/users|{\"username\":\"mallory\"}" \
      "PUT|/users/$dev/reset-password|{\"type\":\"password\",\"value\":\"Mallory-password-1\"}" \
      "PUT|/users/$dev|{\"enabled\":false}" "PUT||{\"registrationAllowed\":true}"; do
    IFS='|' read -r method path body <<<"$probe"
    code="$(wk "$method" "$path" "$body")"
    [[ "$code" == 403 || "$code" == 404 ]] || die "the worker may: $method $path (status $code)"
  done
  # A subgroup with the name of the administrator group gives no rights,
  # because the services compare the full path /squared-admins.
  probe="e2e-probe-$TENANT"
  [[ "$(wk POST /groups "{\"name\":\"$probe\"}")" == 201 ]] || die "the worker cannot make a group"
  sub="$(curl -fsS -H "Authorization: Bearer $wt" "$a/groups?search=$probe&exact=true" | jq -r '.[0].id')"
  wk POST "/groups/$sub/children" '{"name":"squared-admins"}' >/dev/null
  wk PUT "/users/$dev/groups/$(curl -fsS -H "Authorization: Bearer $at" "$a/groups/$sub/children" | jq -r '.[0].id')" >/dev/null
  curl -fsS -H "Authorization: Bearer $(kc_token dev "$DEV_PASSWORD")" "${SQUARED_URL}/api/v1/me" | jq -e '.admin == false' >/dev/null \
    || die "a subgroup named squared-admins gives administrator rights"
  curl -fsS -o /dev/null -X DELETE -H "Authorization: Bearer $at" "$a/groups/$sub"
  log "the worker can change group members, but not users, the realm or protected groups"
}

# guard_status prints the status of the last run of the guard.
guard_status() {
  local pf out
  k -n squared-guard port-forward deployment/squared-guard 18082:8082 >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 1 20); do
    out="$(curl -fsS --max-time 2 http://127.0.0.1:18082/status 2>/dev/null)" && break
    sleep 1
  done
  kill "$pf" 2>/dev/null || true
  wait "$pf" 2>/dev/null || true
  echo "$out"
}

# worker_may_add_dev GROUP prints the status code of the request of the
# worker to add dev to a group.
worker_may_add_dev() {
  local a="${KEYCLOAK_URL}/admin/realms/squared" wt at dev gid
  wt="$(curl -fsS -u "squared-worker:$(secret keycloak-squared-worker-client-secret)" \
    "${KEYCLOAK_URL}/realms/squared/protocol/openid-connect/token" -d grant_type=client_credentials | jq -r .access_token)"
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  dev="$(curl -fsS -H "Authorization: Bearer $at" "$a/users?username=dev&exact=true" | jq -r '.[0].id')"
  gid="$(curl -fsS -H "Authorization: Bearer $at" "$a/groups?search=$1&exact=true" | jq -r --arg n "$1" '.[] | select(.name == $n) | .id')"
  [[ -n "$gid" ]] || { echo "missing"; return; }
  curl -sS -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $wt" "$a/users/$dev/groups/$gid"
  # Undo the change, if the worker could do it.
  curl -sS -o /dev/null -X DELETE -H "Authorization: Bearer $at" "$a/users/$dev/groups/$gid"
}

guard_protected() { [[ "$(worker_may_add_dev "$1")" == 403 ]]; }
guard_clean() { guard_status | jq -e '(.protected | length > 0) and ((.errors // []) | length == 0)' >/dev/null 2>&1; }
# guard_clean_since SECONDS tells if the last guard run was after the Unix
# time SECONDS and had no errors.
guard_clean_since() {
  guard_status | jq -e --argjson t "$1" \
    '(.time | sub("\\.[0-9]+"; "") | fromdateiso8601) > $t and ((.errors // []) | length == 0)' >/dev/null 2>&1
}
guard_released() { [[ "$(worker_may_add_dev "$1")" == 204 ]]; }

# remove_group NAME removes a top-level Keycloak group.
remove_group() {
  local at gid
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  gid="$(curl -fsS -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/groups?search=$1&exact=true" | jq -r '.[0].id')"
  curl -fsS -o /dev/null -X DELETE -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/groups/$gid"
}

# guard_reports_mapper MAPPER CLIENT PATTERN adds a protocol mapper to a
# Keycloak client, waits until the guard reports an error that matches the
# pattern (jq regex), checks that the protection of /squared-admins stays,
# and removes the mapper.
guard_reports_mapper() {
  local mapper="$1" client_id="$2" pattern="$3" at client id
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  client="$(curl -fsS -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/clients?clientId=$client_id" | jq -r '.[0].id')"
  curl -fsS -X POST -H "Authorization: Bearer $at" -H 'Content-Type: application/json' \
    "${KEYCLOAK_URL}/admin/realms/squared/clients/$client/protocol-mappers/models" -d "$mapper" >/dev/null
  guard_reports() { guard_status | jq -e --arg p "$pattern" '[.errors[]? | select(test($p))] | length > 0' >/dev/null; }
  wait_for "the guard reports the mapper of $client_id" 60 guard_reports
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the mapper change"
  id="$(curl -fsS -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/clients/$client/protocol-mappers/models" \
    | jq -r '.[] | select(.name == "e2e-guard") | .id')"
  curl -fsS -X DELETE -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/clients/$client/protocol-mappers/models/$id" >/dev/null
  wait_for "the guard has no errors after the mapper is removed" 60 guard_clean
}

# guard_reports_keycloak PATH CHANGE RESTORE PATTERN puts CHANGE to a path
# of the Keycloak admin API of the realm, waits until the guard reports an
# error that matches the pattern (jq regex), checks that the protection of
# /squared-admins stays, and puts RESTORE.
guard_reports_keycloak() {
  local path="$1" change="$2" restore="$3" pattern="$4" at
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  curl -fsS -X PUT -H "Authorization: Bearer $at" -H 'Content-Type: application/json' \
    "${KEYCLOAK_URL}/admin/realms/squared$path" -d "$change" >/dev/null
  guard_reports() { guard_status | jq -e --arg p "$pattern" '[.errors[]? | select(test($p))] | length > 0' >/dev/null; }
  wait_for "the guard reports the change of $pattern" 60 guard_reports
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the Keycloak change"
  curl -fsS -X PUT -H "Authorization: Bearer $at" -H 'Content-Type: application/json' \
    "${KEYCLOAK_URL}/admin/realms/squared$path" -d "$restore" >/dev/null
  wait_for "the guard has no errors after the Keycloak change is undone" 60 guard_clean
}

# guard_reports_app_ini LINE NEW PATTERN replaces the line LINE with NEW in
# app.ini of the running Forgejo, as "kubectl exec" can, waits until the
# guard reports an error that matches the pattern (jq regex), checks that
# the protection of /squared-admins stays, and puts the line back.
guard_reports_app_ini() {
  local line="$1" new="$2" pattern="$3"
  app_ini_replace "$line" "$new" || die "app.ini of Forgejo has no line '$line'"
  guard_reports() { guard_status | jq -e --arg p "$pattern" '[.errors[]? | select(test($p))] | length > 0' >/dev/null; }
  wait_for "the guard reports '$new' in app.ini" 60 guard_reports
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the app.ini change"
  app_ini_replace "$new" "$line" || die "cannot put the line '$line' back into app.ini of Forgejo"
  wait_for "the guard has no errors after app.ini is back" 60 guard_clean
}

# app_ini_replace LINE NEW replaces the line LINE (the exact text, not a
# pattern) with NEW in app.ini of the running Forgejo. It writes the file in
# place, thus the owner and the mode stay. All steps run in the pod: the
# standard input of "kubectl exec" can lose the data, and an empty app.ini
# stays.
app_ini_replace() {
  k -n forgejo exec deploy/forgejo -c forgejo -- sh -c '
    tmp=/tmp/app.ini.e2e
    awk -v a="$1" -v b="$2" '"'"'$0 == a { print b; found = 1; next } { print } END { if (!found) exit 1 }'"'"' "$0" > "$tmp" &&
      [ -s "$tmp" ] && cat "$tmp" > "$0"
    rc=$?
    rm -f "$tmp"
    exit $rc' /data/gitea/conf/app.ini "$1" "$2"
}

check_guard() {
  local st service group="e2e-ops-$TENANT" policy
  # All four services refer to the administrator group.
  wait_for "the guard has run without errors" 60 guard_clean
  st="$(guard_status)"
  for service in "Argo CD" Harbor Forgejo Squared; do
    jq -e --arg s "$service" '.protected[] | select(.path == "/squared-admins") | .references[] | select(.service == $s)' <<<"$st" >/dev/null \
      || die "the guard does not see that $service gives rights to /squared-admins: $(jq -c '[.protected[] | {path, services: [.references[].service]}], .errors' <<<"$st")"
  done
  # A new line in the Argo CD RBAC makes the group and protects it.
  policy="$(k -n argocd get configmap argocd-rbac-cm -o jsonpath='{.data.policy\.csv}')"
  k -n argocd patch configmap argocd-rbac-cm --type merge \
    -p "$(jq -n --arg p "$policy"$'\ng, /'"$group"$', role:readonly\n' '{data: {"policy.csv": $p}}')" >/dev/null
  wait_for "the guard made and protected /$group" 90 guard_protected "$group"
  # Without the line, the group is not protected anymore.
  k -n argocd patch configmap argocd-rbac-cm --type merge -p "$(jq -n --arg p "$policy" '{data: {"policy.csv": $p}}')" >/dev/null
  wait_for "the guard released /$group" 90 guard_released "$group"
  remove_group "$group"
  log "the guard follows the Argo CD RBAC configuration"

  # A "g" line in the policies of a project role, as kubectl can write it.
  # Argo CD adds these lines to its RBAC without a check.
  local pgroup="e2e-argo-project-$TENANT" project="e2e-guard-$TENANT"
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: $project
  namespace: argocd
spec:
  roles:
    - name: e2e
      policies:
        - "g, /$pgroup, role:readonly"
YAML
  wait_for "the guard made and protected /$pgroup" 90 guard_protected "$pgroup"
  k -n argocd delete appproject "$project" >/dev/null
  wait_for "the guard released /$pgroup" 90 guard_released "$pgroup"
  remove_group "$pgroup"
  log "the guard reads the policies of Argo CD project roles"

  # A claim in the Argo CD scopes that the guard does not know. The guard
  # must report it and keep the protection.
  k -n argocd patch configmap argocd-rbac-cm --type merge -p '{"data": {"scopes": "[groups, roles]"}}' >/dev/null
  guard_reports_scopes() { guard_status | jq -e '[.errors[]? | select(test("scopes has the claim roles"))] | length > 0' >/dev/null; }
  wait_for "the guard reports the unknown claim in the Argo CD scopes" 60 guard_reports_scopes
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the scopes change"
  k -n argocd patch configmap argocd-rbac-cm --type merge -p '{"data": {"scopes": "[groups]"}}' >/dev/null
  wait_for "the guard has no errors after the scopes are back" 60 guard_clean
  log "the guard checks the claims in the Argo CD scopes"

  # A credential template that ends in the host name: Argo CD would also
  # send its credentials to other hosts that start with it. The password is
  # not a real one.
  local argo_api="http://localhost:30081/api/v1" argo_session template="http://forgejo.localhost:30300"
  argo_session="$(curl -fsS "$argo_api/session" -H 'Content-Type: application/json' -d "$(jq -n \
    --arg p "$(k -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)" \
    '{username: "admin", password: $p}')" | jq -er .token)" || die "cannot sign in to Argo CD as admin"
  curl -fsS -X POST -H "Authorization: Bearer $argo_session" -H 'Content-Type: application/json' "$argo_api/repocreds" \
    -d "$(jq -n --arg u "$template" '{url: $u, username: "e2e-guard", password: "not-a-secret"}')" >/dev/null \
    || die "cannot add a credential template to Argo CD"
  guard_reports_template() {
    guard_status | jq -e '[.errors[]? | select(test("credential template http://forgejo[.]localhost:30300 ends in the host name"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the credential template that ends in the host" 60 guard_reports_template
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the credential template"
  curl -fsS -X DELETE -H "Authorization: Bearer $argo_session" -H 'Content-Type: application/json' \
    "$argo_api/repocreds/$(jq -rn --arg u "$template" '$u|@uri')" >/dev/null || die "cannot remove the credential template from Argo CD"
  wait_for "the guard has no errors after the credential template is removed" 60 guard_clean
  log "the guard checks the repository credentials of Argo CD"

  # Another cluster without the check of TLS certificates, as a cluster
  # Secret. Squared does not deploy there, thus the guard warns. The token
  # is not a real one.
  k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: e2e-guard-cluster
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: e2e-guard
  server: https://e2e-guard.invalid:6443
  config: '{"bearerToken": "not-a-secret", "tlsClientConfig": {"insecure": true}}'
YAML
  guard_warns_cluster() {
    guard_status | jq -e '[.warnings[]? | select(test("cluster https://e2e-guard[.]invalid:6443 [(]e2e-guard[)] does not check the TLS certificate"))] | length > 0' >/dev/null
  }
  guard_quiet_cluster() { ! guard_warns_cluster && guard_clean; }
  wait_for "the guard warns about the cluster without TLS checks" 60 guard_warns_cluster
  k -n argocd delete secret e2e-guard-cluster >/dev/null
  wait_for "the guard has no warning after the cluster is removed" 60 guard_quiet_cluster
  log "the guard checks the clusters of Argo CD"

  # A tenant project that permits all namespaces, and an application in it
  # that does not sync by itself. Both have the labels of Squared, as a
  # change by hand of the real objects has. Also an application of the
  # project default with the platform repository, in another notation. The
  # tenant e2eguard does not exist, and the applications have no automatic
  # sync, thus Argo CD deploys nothing.
  local guard_repo="http://forgejo.localhost:30300/squared/tenant-e2eguard.git"
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: tenant-e2eguard
  namespace: argocd
  labels: { app.kubernetes.io/managed-by: squared, squared.dev/tenant: e2eguard }
spec:
  sourceRepos: ["$guard_repo"]
  destinations: [{ server: "https://kubernetes.default.svc", namespace: "*" }]
  clusterResourceWhitelist: []
  namespaceResourceWhitelist: [{ group: apps, kind: Deployment }]
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: e2eguard-dev-web
  namespace: argocd
  labels: { app.kubernetes.io/managed-by: squared, squared.dev/managed: "true", squared.dev/tenant: e2eguard }
spec:
  project: tenant-e2eguard
  source: { repoURL: "$guard_repo", targetRevision: "$(printf '0%.0s' {1..40})", path: deployments/dev/web }
  destination: { server: "https://kubernetes.default.svc", namespace: e2eguard-dev }
  syncPolicy: { syncOptions: [CreateNamespace=false, PruneLast=true] }
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: e2e-guard-copy
  namespace: argocd
spec:
  project: default
  source: { repoURL: "http://Forgejo.localhost:30300/squared/platform", targetRevision: main, path: tenants }
  destination: { server: "https://kubernetes.default.svc", namespace: e2eguard-dev }
YAML
  guard_reports_tenant_argo() {
    guard_status | jq -e '[.errors[]? | select(test("project tenant-e2eguard: a destination has the namespace \"[*]\""))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("application e2eguard-dev-web of tenant e2eguard: it does not sync automatically"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("application e2e-guard-copy uses the GitOps repository http://Forgejo[.]localhost:30300/squared/platform,"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the tenant project for all namespaces, the application without automatic sync and the copy of the platform repository" 60 guard_reports_tenant_argo
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the changed tenant project"
  k -n argocd delete application e2eguard-dev-web e2e-guard-copy >/dev/null
  k -n argocd delete appproject tenant-e2eguard >/dev/null
  wait_for "the guard has no errors after the tenant project and the application are removed" 60 guard_clean
  log "the guard checks the tenant projects, the applications and their sources in Argo CD"

  # The root application without selfHeal: a change by hand of a tenant
  # project stays.
  k -n argocd patch application squared-platform --type merge -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":false}}}}' >/dev/null
  guard_reports_root() {
    guard_status | jq -e '[.errors[]? | select(test("root application squared-platform: selfHeal is off"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the root application without selfHeal" 60 guard_reports_root
  k -n argocd patch application squared-platform --type merge -p '{"spec":{"syncPolicy":{"automated":{"selfHeal":true}}}}' >/dev/null
  wait_for "the guard has no errors after selfHeal is on again" 60 guard_clean
  log "the guard checks the sync policy of the root application"

  # Argo CD reads applications also in the namespace default, and an
  # application there has the platform repository. The project default
  # does not permit applications from other namespaces, thus Argo CD
  # deploys nothing.
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"application.namespaces":"default"}}' >/dev/null
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: e2e-guard-elsewhere
  namespace: default
spec:
  project: default
  source: { repoURL: "http://forgejo.localhost:30300/squared/platform.git", targetRevision: main, path: tenants }
  destination: { server: "https://kubernetes.default.svc", namespace: default }
YAML
  guard_reports_app_namespace() {
    guard_status | jq -e '[.errors[]? | select(test("application.namespaces [(]argocd-cmd-params-cm[)] has the namespace \"default\""))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("the application default/e2e-guard-elsewhere uses the GitOps repository"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the application namespace and the application in it" 60 guard_reports_app_namespace
  k -n default delete application e2e-guard-elsewhere >/dev/null
  k -n argocd patch configmap argocd-cmd-params-cm --type json -p '[{"op":"remove","path":"/data/application.namespaces"}]' >/dev/null
  wait_for "the guard has no errors after the application namespace is removed" 60 guard_clean
  log "the guard checks the namespaces in which Argo CD reads applications"

  # An ApplicationSet that reads the platform repository, with a project
  # from the parameters, a pull request generator that sends a token
  # without the check of the TLS certificate, and a template patch with the
  # finalizer of Argo CD and a tenant project in a branch. The Git
  # generator has a template with another tenant project. No directory
  # matches the path, and the SCM generators are off, thus it makes no
  # applications.
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: e2e-guard-set
  namespace: argocd
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: "http://forgejo.localhost:30300/squared/platform.git"
        revision: main
        directories: [{ path: e2e-none/* }]
        template:
          metadata: {}
          spec: { project: tenant-e2egen, destination: { namespace: e2e-guard } }
    - pullRequest:
        gitea: { api: "https://git.example.invalid", owner: e2e, repo: e2e, insecure: true, tokenRef: { secretName: e2e-guard, key: token } }
  template:
    metadata: { name: "e2e-guard-{{.path.basename}}" }
    spec:
      project: "{{.path.basename}}"
      source: { repoURL: "https://git.example.invalid/e2e.git", targetRevision: main, path: "{{.path.path}}" }
      destination: { server: "https://kubernetes.default.svc", namespace: e2e-guard }
  templatePatch: |
    metadata:
      finalizers: [resources-finalizer.argocd.argoproj.io]
    spec:
    {{- if eq .path.basename "shop" }}
      project: tenant-e2eguard
    {{- end }}
      destination: { namespace: e2e-guard }
YAML
  guard_reports_appset() {
    guard_status | jq -e '[.errors[]? | select(test("the ApplicationSet e2e-guard-set: a generator reads the GitOps repository"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: the project .* of its applications comes from the generators"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: a generator sends credentials to https://git.example.invalid without the check"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: preserveResourcesOnDeletion is not true"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: in its templatePatch, its applications get the finalizer resources-finalizer"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: in its templatePatch, its applications use the tenant project tenant-e2eguard"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("e2e-guard-set: in the template of a git generator, its applications use the tenant project tenant-e2egen"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the ApplicationSet with the platform repository and a project from the parameters" 60 guard_reports_appset
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the ApplicationSet"
  k -n argocd delete applicationset e2e-guard-set >/dev/null
  wait_for "the guard has no errors after the ApplicationSet is removed" 60 guard_clean
  log "the guard checks the ApplicationSets of Argo CD"

  # A plugin generator with a ConfigMap that takes its token from another
  # key than a plugin token and sends it without TLS, and one with a
  # ConfigMap that does not exist. The Secret does not exist and the host
  # is not real, thus the controller sends nothing. Also a cluster decision
  # resource generator with the status of Argo CD applications, and one with
  # a ConfigMap name from a list. The application does not exist, thus the
  # generators make no applications.
  k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-guard-plugin
  namespace: argocd
data:
  token: "\$e2e-guard-missing:token"
  baseUrl: "http://plugin.example.invalid"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: e2e-guard-decision
  namespace: argocd
data: { apiVersion: argoproj.io/v1alpha1, kind: applications, statusListKey: resources, matchKey: name }
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: e2e-guard-plugins
  namespace: argocd
spec:
  goTemplate: true
  syncPolicy: { applicationsSync: create-only, preserveResourcesOnDeletion: true }
  generators:
    - plugin: { configMapRef: { name: e2e-guard-plugin }, input: { parameters: { id: "1" } } }
    - plugin: { configMapRef: { name: e2e-guard-none } }
    - clusterDecisionResource: { configMapRef: e2e-guard-decision, name: e2e-none }
    - matrix:
        generators:
          - list: { elements: [{ cm: e2e-guard-decision }] }
          - clusterDecisionResource: { configMapRef: "{{.cm}}", name: e2e-none }
  template:
    metadata: { name: "e2e-guard-plugin-{{.id}}" }
    spec:
      project: default
      source: { repoURL: "https://git.example.invalid/e2e.git", targetRevision: main, path: e2e }
      destination: { server: "https://kubernetes.default.svc", namespace: e2e-guard }
YAML
  guard_reports_plugin() {
    local st
    st="$(guard_status)"
    jq -e '[.errors[]? | select(test("the plugin ConfigMap e2e-guard-plugin of the ApplicationSets e2e-guard-plugins: its token is the key token of the Secret e2e-guard-missing"))] | length > 0' <<<"$st" >/dev/null &&
      jq -e '[.errors[]? | select(test("e2e-guard-plugin of the ApplicationSets e2e-guard-plugins: it sends its token to http://plugin.example.invalid without TLS"))] | length > 0' <<<"$st" >/dev/null &&
      jq -e '[.warnings[]? | select(test("the ApplicationSets e2e-guard-plugins use the plugin ConfigMap e2e-guard-none, which does not exist"))] | length > 0' <<<"$st" >/dev/null &&
      jq -e '[.errors[]? | select(test("the cluster decision resource ConfigMap e2e-guard-decision of the ApplicationSets e2e-guard-plugins: it takes the decisions from the status of the Argo CD applications"))] | length > 0' <<<"$st" >/dev/null &&
      jq -e '[.errors[]? | select(test("e2e-guard-plugins: the ConfigMap .* of a clusterDecisionResource generator comes from other generators"))] | length > 0' <<<"$st" >/dev/null
  }
  wait_for "the guard reports the ConfigMaps of the plugin and cluster decision resource generators" 60 guard_reports_plugin
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the plugin generator"
  k -n argocd delete applicationset e2e-guard-plugins >/dev/null
  k -n argocd delete configmap e2e-guard-plugin e2e-guard-decision >/dev/null
  wait_for "the guard has no errors after the generators are removed" 60 guard_clean
  log "the guard checks the plugin and cluster decision resource generators of the ApplicationSets"

  # An ApplicationSet that makes an application with the name of an
  # application of Squared takes it over, also with the policy create-only.
  # The root application then puts the labels of Squared back, as here.
  # With preserveResourcesOnDeletion, the removal of the ApplicationSet
  # removes only the application.
  local taken=e2etakeover-dev-web
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: $taken
  namespace: argocd
  labels: { app.kubernetes.io/managed-by: squared, squared.dev/managed: "true", squared.dev/tenant: e2etakeover }
spec:
  project: tenant-e2etakeover
  source: { repoURL: "http://forgejo.localhost:30300/squared/tenant-e2etakeover.git", targetRevision: main, path: dev/web }
  destination: { server: "https://kubernetes.default.svc", namespace: e2etakeover-dev }
YAML
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: e2e-guard-taker
  namespace: argocd
spec:
  syncPolicy: { applicationsSync: create-only, preserveResourcesOnDeletion: true }
  generators: [{ list: { elements: [{ id: "1" }] } }]
  template:
    metadata: { name: $taken }
    spec:
      project: default
      source: { repoURL: "https://git.example.invalid/e2e.git", targetRevision: main, path: e2e }
      destination: { server: "https://kubernetes.default.svc", namespace: e2e-guard }
YAML
  app_taken() { [[ "$(k -n argocd get application "$taken" -o jsonpath='{.metadata.ownerReferences[0].name}')" == e2e-guard-taker ]]; }
  wait_for "the ApplicationSet takes over the application" 60 app_taken
  k -n argocd label application "$taken" app.kubernetes.io/managed-by=squared squared.dev/managed=true squared.dev/tenant=e2etakeover >/dev/null
  guard_reports_takeover() {
    guard_status | jq -e --arg a "$taken" '[.errors[]? | select(test("the application \($a) of tenant e2etakeover: the ApplicationSet e2e-guard-taker owns it"))] | length > 0' >/dev/null &&
      guard_status | jq -e --arg a "$taken" '[.errors[]? | select(test("e2e-guard-taker: it makes the application \($a), thus it takes over"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the application that the ApplicationSet took over" 60 guard_reports_takeover
  k -n argocd delete applicationset e2e-guard-taker >/dev/null
  app_gone() { ! k -n argocd get application "$taken" >/dev/null 2>&1; }
  wait_for "Kubernetes removes the application with the ApplicationSet" 60 app_gone
  wait_for "the guard has no errors after the ApplicationSet is removed" 60 guard_clean
  log "the guard reports applications of Squared that an ApplicationSet takes over"

  # The SCM generators on, without a list of servers and without the strict
  # mode for the token Secrets.
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"applicationsetcontroller.enable.scm.providers":"true"}}' >/dev/null
  guard_reports_scm() {
    guard_status | jq -e '[.errors[]? | select(test("allowed.scm.providers is empty"))] | length > 0' >/dev/null &&
      guard_status | jq -e '[.errors[]? | select(test("enable.tokenref.strict.mode is not true"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the SCM generators without limits" 60 guard_reports_scm
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"applicationsetcontroller.enable.scm.providers":"false"}}' >/dev/null
  wait_for "the guard has no errors after the SCM generators are off" 60 guard_clean
  log "the guard checks the SCM settings of the ApplicationSet controller"

  # The policy sync for the controller. Then create-only with the override
  # on, and an ApplicationSet with the policy sync. Its list is empty, thus
  # it makes no applications.
  local params='{"data":{"applicationsetcontroller.policy":"create-only","applicationsetcontroller.enable.policy.override":"false"}}'
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"applicationsetcontroller.policy":"sync"}}' >/dev/null
  guard_reports_policy() { guard_status | jq -e '[.errors[]? | select(test("has the policy sync in applicationsetcontroller.policy"))] | length > 0' >/dev/null; }
  wait_for "the guard reports the policy sync of the ApplicationSet controller" 60 guard_reports_policy
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p '{"data":{"applicationsetcontroller.policy":"create-only","applicationsetcontroller.enable.policy.override":"true"}}' >/dev/null
  k apply -f - >/dev/null <<YAML
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: e2e-guard-updater
  namespace: argocd
spec:
  syncPolicy: { applicationsSync: sync, preserveResourcesOnDeletion: true }
  generators: [{ list: { elements: [] } }]
  template:
    metadata: { name: e2e-guard-updater }
    spec:
      project: default
      source: { repoURL: "https://git.example.invalid/e2e.git", targetRevision: main, path: e2e }
      destination: { server: "https://kubernetes.default.svc", namespace: e2e-guard }
YAML
  guard_reports_set_policy() {
    guard_status | jq -e '[.errors[]? | select(test("e2e-guard-updater: its policy sync permits updates of its applications"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the ApplicationSet with the policy sync" 60 guard_reports_set_policy
  k -n argocd delete applicationset e2e-guard-updater >/dev/null
  k -n argocd patch configmap argocd-cmd-params-cm --type merge -p "$params" >/dev/null
  wait_for "the guard has no errors after the policy is back" 60 guard_clean
  log "the guard checks the policy of the ApplicationSet controller"

  # Read rights in another namespace for the group of the service accounts
  # of the namespace argocd, which has the ApplicationSet controller.
  k apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: e2e-guard-appset
  namespace: default
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: view }
subjects:
  - { apiGroup: rbac.authorization.k8s.io, kind: Group, name: "system:serviceaccounts:argocd" }
YAML
  guard_reports_appset_rbac() {
    guard_status | jq -e '[.errors[]? | select(test("the RoleBinding default/e2e-guard-appset [(]ClusterRole view[)] gives the ApplicationSet controller .* in the namespace default, more than Argo CD needs"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the rights of the ApplicationSet controller in another namespace" 60 guard_reports_appset_rbac
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the RoleBinding"
  k -n default delete rolebinding e2e-guard-appset >/dev/null
  wait_for "the guard has no errors after the RoleBinding is removed" 60 guard_clean
  log "the guard checks the rights of the ApplicationSet controller"

  # The application controller has only the rights that the projects
  # need: it cannot make ClusterRoleBindings or read Secrets outside its
  # namespace. Then it gets read rights on Secrets in the cluster, and the
  # guard must report them.
  local ctl=system:serviceaccount:argocd:argocd-application-controller
  [[ "$(k auth can-i create clusterrolebindings -A --as "$ctl")" == no ]] || die "the application controller can make ClusterRoleBindings"
  [[ "$(k auth can-i list secrets -n kube-system --as "$ctl")" == no ]] || die "the application controller can read the Secrets of kube-system"
  [[ "$(k auth can-i create deployments -n default --as "$ctl")" == yes ]] || die "the application controller cannot make Deployments"
  k apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: e2e-guard-secrets
rules:
  - { apiGroups: [""], resources: [secrets], verbs: [get, list] }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: e2e-guard-secrets
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: e2e-guard-secrets }
subjects:
  - { kind: ServiceAccount, name: argocd-application-controller, namespace: argocd }
YAML
  guard_reports_app_rbac() {
    guard_status | jq -e '[.errors[]? | select(test("the ClusterRoleBinding e2e-guard-secrets [(]ClusterRole e2e-guard-secrets[)] gives the application controller get list on secrets [(]core[)] in the cluster, more than the projects permit"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the read rights of the application controller on Secrets" 60 guard_reports_app_rbac
  k delete clusterrolebinding e2e-guard-secrets >/dev/null
  k delete clusterrole e2e-guard-secrets >/dev/null
  wait_for "the guard has no errors after the ClusterRoleBinding is removed" 60 guard_clean
  log "the guard checks the rights of the application controller"

  # The API server gets the right to run commands in pods.
  k apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: e2e-guard-exec
rules:
  - { apiGroups: [""], resources: [pods/exec], verbs: [create] }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: e2e-guard-exec
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: e2e-guard-exec }
subjects:
  - { kind: ServiceAccount, name: argocd-server, namespace: argocd }
YAML
  guard_reports_server_rbac() {
    guard_status | jq -e '[.errors[]? | select(test("the ClusterRoleBinding e2e-guard-exec [(]ClusterRole e2e-guard-exec[)] gives the API server create on pods/exec [(]core[)] in the cluster, more than the projects permit"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the right of the API server to run commands in pods" 60 guard_reports_server_rbac
  k delete clusterrolebinding e2e-guard-exec >/dev/null
  k delete clusterrole e2e-guard-exec >/dev/null
  wait_for "the guard has no errors after the ClusterRoleBinding is removed" 60 guard_clean
  log "the guard checks the rights of the API server"

  # The repository server has no token of the Kubernetes API. Then its pods
  # get one, and the guard must report it.
  ! k -n argocd exec deploy/argocd-repo-server -c repo-server -- ls /var/run/secrets/kubernetes.io/serviceaccount >/dev/null 2>&1 \
    || die "the repository server has a token of the Kubernetes API"
  k -n argocd patch deployment argocd-repo-server --type merge -p '{"spec":{"template":{"spec":{"automountServiceAccountToken":true}}}}' >/dev/null
  guard_reports_repo_token() {
    guard_status | jq -e '[.errors[]? | select(test("the repository server of Argo CD has rights that it does not need: its pods get a token of the Kubernetes API"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the token of the repository server" 60 guard_reports_repo_token
  k -n argocd patch deployment argocd-repo-server --type merge -p '{"spec":{"template":{"spec":{"automountServiceAccountToken":false}}}}' >/dev/null
  k -n argocd rollout status deployment/argocd-repo-server --timeout 5m >/dev/null
  wait_for "the guard has no errors after the token is removed" 60 guard_clean
  log "the guard checks the rights and the token of the repository server"

  # Keycloak mappers that write the groups claim or the email claim. These
  # are in no token, thus they are harmless, but the guard must report
  # each mapper that does not come from the group membership or the email
  # address of the user.
  guard_reports_mapper "$(jq -n '{name: "e2e-guard", protocol: "openid-connect", protocolMapper: "oidc-hardcoded-claim-mapper",
    config: {"claim.name": "groups", "claim.value": "/e2e", "id.token.claim": "false", "access.token.claim": "false",
      "userinfo.token.claim": "false"}}')" argocd 'mapper "e2e-guard" \(type oidc-hardcoded-claim-mapper\) writes the group claim groups'
  guard_reports_mapper "$(jq -n '{name: "e2e-guard", protocol: "openid-connect", protocolMapper: "oidc-usermodel-attribute-mapper",
    config: {"claim.name": "email", "user.attribute": "nickname", "id.token.claim": "false", "access.token.claim": "false",
      "userinfo.token.claim": "false"}}')" forgejo 'mapper "e2e-guard" \(type oidc-usermodel-attribute-mapper\) writes the email claim email'
  log "the guard checks the group and email mappers of the Keycloak service clients"

  # Realm settings that let a person choose an email address. Keycloak
  # turns off the sign-in with the address when it permits duplicates, thus
  # the restore turns it on again.
  guard_reports_keycloak "" '{"duplicateEmailsAllowed": true}' '{"duplicateEmailsAllowed": false, "loginWithEmailAllowed": true}' \
    'duplicateEmailsAllowed is on'
  local profile at
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  profile="$(curl -fsS -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/users/profile")"
  guard_reports_keycloak /users/profile \
    "$(jq '.attributes |= map(if .name == "email" then .permissions.edit = ["admin", "user"] else . end)' <<<"$profile")" \
    "$profile" 'users can change their address'
  log "the guard checks the email settings of the Keycloak realm"

  # A change of the administrator group in Forgejo, as in the web
  # interface. The Forgejo command line writes the same database table.
  local fgroup="e2e-forgejo-$TENANT" auth_id
  forgejo_cli() { k -n forgejo exec deploy/forgejo -c forgejo -- gitea "$@"; }
  auth_id="$(forgejo_cli admin auth list | awk '$2 == "keycloak" {print $1}')"
  [[ -n "$auth_id" ]] || die "Forgejo has no authentication source keycloak"
  forgejo_cli admin auth update-oauth --id "$auth_id" --admin-group "/$fgroup" >/dev/null
  wait_for "the guard made and protected /$fgroup" 90 guard_protected "$fgroup"
  forgejo_cli admin auth update-oauth --id "$auth_id" --admin-group "/squared-admins" >/dev/null
  wait_for "the guard released /$fgroup" 90 guard_released "$fgroup"
  remove_group "$fgroup"
  log "the guard follows the Forgejo authentication sources, also changes outside the Helm chart"

  # A Forgejo upgrade that renames a field. The guard must report it and
  # keep the protection, not read an empty value.
  forgejo_sql() { k -n forgejo exec -i statefulset/forgejo-db -- psql -q -U forgejo -d forgejo -v ON_ERROR_STOP=1 -c "$1" >/dev/null; }
  forgejo_sql "UPDATE login_source SET cfg = (cfg::jsonb - 'AdminGroup' || jsonb_build_object('AdministratorGroup', cfg::jsonb -> 'AdminGroup'))::text WHERE name = 'keycloak'"
  guard_reports_rename() { guard_status | jq -e '[.errors[]? | select(test("AdminGroup is missing"))] | length > 0' >/dev/null; }
  wait_for "the guard reports the renamed Forgejo field" 60 guard_reports_rename
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the renamed field"
  forgejo_sql "UPDATE login_source SET cfg = (cfg::jsonb - 'AdministratorGroup' || jsonb_build_object('AdminGroup', cfg::jsonb -> 'AdministratorGroup'))::text WHERE name = 'keycloak'"
  wait_for "the guard has no errors after the field is back" 60 guard_clean
  log "the guard reports renamed Forgejo fields and keeps the protection"

  # Unsafe sign-in settings in app.ini, as "kubectl exec" can write them.
  # Forgejo reads the file only at its start, thus the guard reports the
  # values for the next start.
  guard_reports_app_ini 'ACCOUNT_LINKING = disabled' 'ACCOUNT_LINKING = auto' 'ACCOUNT_LINKING is auto: [^;]*[(]after the next start[)]'
  guard_reports_app_ini 'ALLOW_ONLY_EXTERNAL_REGISTRATION = true' 'ALLOW_ONLY_EXTERNAL_REGISTRATION = false' \
    'local registration is open[^;]*[(]after the next start[)]'
  guard_reports_app_ini 'ENABLE_INTERNAL_SIGNIN = false' 'ENABLE_INTERNAL_SIGNIN = true' \
    'ENABLE_INTERNAL_SIGNIN is on[^;]*[(]after the next start[)]'
  guard_reports_app_ini 'ALLOWED_HOST_LIST = external,argocd-server.argocd.svc.cluster.local' 'ALLOWED_HOST_LIST = *.svc.cluster.local' \
    'ALLOWED_HOST_LIST of [[]webhook[]] has [*][.]svc[.]cluster[.]local[^;]*[(]after the next start[)]'
  # [migrations] is not in app.ini of the development environment (the
  # defaults are safe), thus the test adds the section at the end.
  local ini=/data/gitea/conf/app.ini
  k -n forgejo exec deploy/forgejo -c forgejo -- sh -c "! grep -q '^\[migrations\]' $ini && printf '\n[migrations]\nALLOW_LOCALNETWORKS = true\n' >> $ini" \
    || die "cannot add [migrations] to app.ini of Forgejo"
  guard_reports_migrations() {
    guard_status | jq -e '[.errors[]? | select(test("ALLOW_LOCALNETWORKS of [[]migrations[]] is on[^;]*[(]after the next start[)]"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports ALLOW_LOCALNETWORKS in app.ini" 60 guard_reports_migrations
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the app.ini change"
  k -n forgejo exec deploy/forgejo -c forgejo -- sh -c "sed -i '/^\[migrations\]\$/,\$d' $ini"
  wait_for "the guard has no errors after app.ini is back" 60 guard_clean
  # IMPORT_LOCAL_PATHS is not in app.ini either: add it to [security].
  k -n forgejo exec deploy/forgejo -c forgejo -- sh -c "! grep -q '^IMPORT_LOCAL_PATHS' $ini && sed -i '/^\[security\]\$/a IMPORT_LOCAL_PATHS = true' $ini" \
    || die "cannot add IMPORT_LOCAL_PATHS to app.ini of Forgejo"
  guard_reports_import() {
    guard_status | jq -e '[.errors[]? | select(test("IMPORT_LOCAL_PATHS of [[]security[]] is on[^;]*[(]after the next start[)]"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports IMPORT_LOCAL_PATHS in app.ini" 60 guard_reports_import
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the app.ini change"
  app_ini_replace 'IMPORT_LOCAL_PATHS = true' '' || die "cannot remove IMPORT_LOCAL_PATHS from app.ini of Forgejo"
  wait_for "the guard has no errors after app.ini is back" 60 guard_clean
  # A webhook delivery without the check of TLS certificates.
  k -n forgejo exec deploy/forgejo -c forgejo -- sh -c "! grep -q '^SKIP_TLS_VERIFY' $ini && sed -i '/^\[webhook\]\$/a SKIP_TLS_VERIFY = true' $ini" \
    || die "cannot add SKIP_TLS_VERIFY to app.ini of Forgejo"
  guard_reports_tls() {
    guard_status | jq -e '[.errors[]? | select(test("SKIP_TLS_VERIFY of [[]webhook[]] is on[^;]*[(]after the next start[)]"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports SKIP_TLS_VERIFY in app.ini" 60 guard_reports_tls
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the app.ini change"
  app_ini_replace 'SKIP_TLS_VERIFY = true' '' || die "cannot remove SKIP_TLS_VERIFY from app.ini of Forgejo"
  wait_for "the guard has no errors after app.ini is back" 60 guard_clean
  log "the guard checks the sign-in, registration, webhook, migration and TLS settings of Forgejo"

  # A deploy key that can push to the platform repository, as a Forgejo
  # administrator can add it. A public key only; nobody has the private key.
  local key_id
  key_id="$(fj_admin /repos/squared/platform/keys -X POST -H 'Content-Type: application/json' -d '{"title": "e2e-guard",
    "read_only": false, "key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPjQQV11qooUqSWyRghuqLxvZm203ZRLZWyPFj7/k9Hh squared-e2e-deploy"}' | jq -r .id)"
  [[ "$key_id" =~ ^[0-9]+$ ]] || die "cannot add a deploy key to squared/platform"
  guard_reports_deploy_key() {
    guard_status | jq -e '[.errors[]? | select(test("deploy key \"e2e-guard\" of squared/platform [^;]*can push"))] | length > 0' >/dev/null
  }
  wait_for "the guard reports the deploy key of squared/platform" 60 guard_reports_deploy_key
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the deploy key"
  fj_admin "/repos/squared/platform/keys/$key_id" -X DELETE >/dev/null
  wait_for "the guard has no errors after the deploy key is removed" 60 guard_clean
  log "the guard checks the deploy keys of the GitOps repositories"

  # Write access for argocd-reader, which may only read: as a collaborator,
  # then through a new team.
  guard_reports_writer() {
    local pattern="$1"
    guard_reports() { guard_status | jq -e --arg p "$pattern" '[.errors[]? | select(test($p))] | length > 0' >/dev/null; }
    wait_for "the guard reports: $pattern" 60 guard_reports
    [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the access change"
  }
  fj_admin /repos/squared/platform/collaborators/argocd-reader -X PUT -H 'Content-Type: application/json' -d '{"permission": "write"}' >/dev/null
  guard_reports_writer 'user argocd-reader is a collaborator of squared/platform and can push'
  fj_admin /repos/squared/platform/collaborators/argocd-reader -X DELETE >/dev/null
  wait_for "the guard has no errors after the collaborator is removed" 60 guard_clean
  local team_id
  team_id="$(fj_admin /orgs/squared/teams -X POST -H 'Content-Type: application/json' \
    -d '{"name": "e2e-writers", "permission": "write", "units": ["repo.code"], "includes_all_repositories": true}' | jq -r .id)"
  [[ "$team_id" =~ ^[0-9]+$ ]] || die "cannot make a Forgejo team"
  fj_admin "/teams/$team_id/members/argocd-reader" -X PUT >/dev/null
  guard_reports_writer 'user argocd-reader is in the team e2e-writers of squared [(]all repositories[)] and can push'
  fj_admin "/teams/$team_id" -X DELETE >/dev/null
  wait_for "the guard has no errors after the team is removed" 60 guard_clean
  log "the guard checks the team and collaborator access to the GitOps repositories"

  # The worker protects the branch of the platform repository: not even a
  # Forgejo administrator can push to it.
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -u "forgejo-admin:$(secret forgejo-admin-password)" -X POST \
    -H 'Content-Type: application/json' "${FORGEJO_URL}/api/v1/repos/squared/platform/contents/e2e-guard.txt" \
    -d '{"content": "ZTJl", "message": "e2e: must fail", "branch": "main"}')"
  [[ "$code" == 403 ]] || die "a Forgejo administrator could write to the platform repository, or the test is wrong (status $code)"
  # A weaker rule, as an administrator can make it.
  fj_admin /repos/squared/platform/branch_protections/main -X PATCH -H 'Content-Type: application/json' -d '{"apply_to_admins": false}' >/dev/null
  guard_reports_writer 'branch main of squared/platform: the rule does not apply to Forgejo administrators'
  fj_admin /repos/squared/platform/branch_protections/main -X PATCH -H 'Content-Type: application/json' -d '{"apply_to_admins": true}' >/dev/null
  wait_for "the guard has no errors after the rule is back" 60 guard_clean
  log "the guard checks the branch protection of the GitOps repositories"

  # A webhook of the platform repository to an address that Squared does
  # not expect. It is inactive, thus Forgejo sends nothing.
  local hook_id
  hook_id="$(fj_admin /repos/squared/platform/hooks -X POST -H 'Content-Type: application/json' -d '{"type": "gitea",
    "config": {"url": "http://e2e-guard.invalid/hook", "content_type": "json"}, "events": ["push"], "active": false}' | jq -r .id)"
  [[ "$hook_id" =~ ^[0-9]+$ ]] || die "cannot add a webhook to squared/platform"
  guard_reports_writer 'a webhook of squared/platform [(]gitea, inactive[)] sends events to e2e-guard.invalid'
  fj_admin "/repos/squared/platform/hooks/$hook_id" -X DELETE >/dev/null
  wait_for "the guard has no errors after the webhook is removed" 60 guard_clean
  log "the guard checks the webhooks of the GitOps repositories"

  # A push mirror of the platform repository, written into the database as
  # Forgejo writes it. It never syncs: no interval, no sync at a commit.
  forgejo_db() { k -n forgejo exec -i statefulset/forgejo-db -- psql -q -U forgejo -d forgejo -v ON_ERROR_STOP=1 -c "$1" >/dev/null; }
  forgejo_db "INSERT INTO push_mirror (repo_id, remote_name, remote_address, sync_on_commit, interval, created_unix)
    SELECT id, 'e2e_guard', 'https://e2e-guard.invalid/platform.git', false, 0, extract(epoch from now())::bigint
    FROM repository WHERE owner_name = 'squared' AND name = 'platform'"
  guard_reports_writer 'squared/platform has a push mirror to e2e-guard[.]invalid'
  forgejo_db "DELETE FROM push_mirror WHERE remote_name = 'e2e_guard'"
  wait_for "the guard has no errors after the push mirror is removed" 60 guard_clean
  log "the guard checks the mirrors of the GitOps repositories"

  # A change of the administrator group in Harbor, through the API as in
  # the web interface.
  local hgroup="e2e-harbor-$TENANT"
  harbor_admin_group() {
    curl -fsS -u "admin:$(secret harbor-admin-password)" -X PUT -H 'Content-Type: application/json' \
      -H 'X-Requested-With: squared' "${HARBOR_URL}/api/v2.0/configurations" \
      -d "$(jq -n --arg g "$1" '{oidc_admin_group: $g}')" >/dev/null
  }
  harbor_admin_group "/$hgroup"
  wait_for "the guard made and protected /$hgroup" 90 guard_protected "$hgroup"
  harbor_admin_group "/squared-admins"
  wait_for "the guard released /$hgroup" 90 guard_released "$hgroup"
  remove_group "$hgroup"
  log "the guard follows the live Harbor configuration"

  # A Harbor upgrade that renames a setting.
  harbor_sql() { k -n harbor exec -i statefulset/harbor-database -c database -- psql -q -U postgres -d registry -v ON_ERROR_STOP=1 -c "$1" >/dev/null; }
  harbor_sql "UPDATE properties SET k = 'oidc_admin_groups' WHERE k = 'oidc_admin_group'"
  guard_reports_harbor() { guard_status | jq -e '[.errors[]? | select(test("oidc_admin_groups can give rights"))] | length > 0' >/dev/null; }
  wait_for "the guard reports the renamed Harbor setting" 60 guard_reports_harbor
  [[ "$(worker_may_add_dev squared-admins)" == 403 ]] || die "the protection of /squared-admins is gone after the renamed setting"
  harbor_sql "UPDATE properties SET k = 'oidc_admin_group' WHERE k = 'oidc_admin_groups'"
  wait_for "the guard has no errors after the setting is back" 60 guard_clean
  log "the guard reports renamed Harbor settings and keeps the protection"
}

kc_token() {
  curl -fsS "${KEYCLOAK_URL}/realms/squared/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=squared-cli -d scope=openid \
    --data-urlencode "username=$1" --data-urlencode "password=$2" | jq -er .access_token
}

step "sign in to the web interface with Keycloak and make an API token"
JAR="$WORK/cookies"
kc_signin "$ADMIN_USER" "$DEV/keycloak-admin-user-password" "$JAR"
CSRF="$(curl -sS -c "$JAR" -b "$JAR" "${SQUARED_URL}/account" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$CSRF" ]] || die "sign-in failed"
TOKEN="$(curl -sS -c "$JAR" -b "$JAR" --data-urlencode "csrf=$CSRF" --data-urlencode "name=e2e $TENANT" \
  --data-urlencode "days=1" "${SQUARED_URL}/account/tokens" | sed -n 's/.*<pre class="token">\([^<]*\)<.*/\1/p')"
[[ "$TOKEN" == sqp_* ]] || die "no API token"
api GET /me | jq -e '.admin == true' >/dev/null

step "use Keycloak access tokens for the API"
ADMIN_JWT="$(kc_token "$ADMIN_USER" "$ADMIN_PASSWORD")"
DEV_JWT="$(kc_token dev "$DEV_PASSWORD")"
curl -fsS -H "Authorization: Bearer $ADMIN_JWT" "${SQUARED_URL}/api/v1/me" | jq -e '.admin == true and .email == "platform-admin@squared.test"' >/dev/null \
  || die "the access token of the administrator does not give administrator rights"
curl -fsS -H "Authorization: Bearer $DEV_JWT" "${SQUARED_URL}/api/v1/me" | jq -e '.admin == false and .email == "dev@squared.test"' >/dev/null \
  || die "the access token of the developer gives the wrong rights"
code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $DEV_JWT" -H 'Content-Type: application/json' \
  -d '{"slug": "forbidden", "name": "No", "owner": "dev@squared.test", "environments": ["dev"]}' "${SQUARED_URL}/api/v1/tenants")"
[[ "$code" == "403" ]] || die "a user without the administrator group made a tenant (status $code)"
log "the administrator group in Keycloak controls the administrator rights"

step "check the limits of the Keycloak account of the worker"
check_worker_limits

step "the guard protects groups that service configurations refer to"
check_guard

step "sign in to Argo CD, Harbor and Forgejo with Keycloak"
check_sso "$ADMIN_USER" "$DEV/keycloak-admin-user-password" true
check_sso dev "$DEV/keycloak-dev-user-password" false

step "Forgejo accounts belong to Keycloak users"
check_forgejo_linking
check_forgejo_passwords
check_forgejo_credentials

step "make tenant $TENANT"
api POST /tenants "$(jq -n --arg s "$TENANT" --arg o "$ADMIN_EMAIL" \
  '{slug: $s, name: ("End-to-end " + $s), owner: $o, environments: ["dev"], quota: {cpu: "2", memory: "2Gi", pods: 10}}')" >/dev/null
wait_for "tenant is ready" 300 tenant_ready
k get namespace "$NS" >/dev/null
k -n "$NS" get secret squared-registry >/dev/null
# The webhook to Argo CD still works with the short ALLOWED_HOST_LIST: the
# last delivery of the organization webhook succeeded.
hook_ok() {
  [[ "$(k -n forgejo exec statefulset/forgejo-db -- psql -U forgejo -d forgejo -Atc \
    "SELECT t.is_succeed FROM hook_task t JOIN webhook w ON w.id = t.hook_id WHERE w.repo_id = 0 AND w.owner_id > 0 AND t.is_delivered ORDER BY t.id DESC LIMIT 1")" == t ]]
}
wait_for "the webhook to Argo CD delivers" 60 hook_ok
fj_admin "/repos/squared/tenant-$TENANT/branch_protections/main" \
  | jq -e '.push_whitelist_usernames == ["squared-bot"] and .merge_whitelist_usernames == ["squared-bot"] and .apply_to_admins' >/dev/null \
  || die "the tenant repository has no branch protection for Squared only"

# The root application repairs a change by hand of the tenant project. The
# application controller must be able to list the AppProjects in the
# cluster for this (resource.respectRBAC).
k -n argocd patch appproject "tenant-$TENANT" --type json \
  -p '[{"op":"add","path":"/spec/sourceRepos/-","value":"https://git.example.invalid/e2e.git"}]' >/dev/null
project_repaired() {
  [[ "$(k -n argocd get appproject "tenant-$TENANT" -o json | jq '.spec.sourceRepos | length')" == 1 ]]
}
wait_for "Argo CD repairs the changed tenant project" 120 project_repaired

step "map tenant members to Harbor, Forgejo and Argo CD"
# dev has no rights in the tenant yet.
[[ "$(dev_access)" == "403 404" || "$(dev_access)" == "404 404" ]] || die "dev has access before the membership: $(dev_access)"
api PUT "/tenants/$TENANT/members" '{"email": "dev@squared.test", "role": "developer"}' >/dev/null
wait_for "dev gets the Harbor project and the Forgejo repository" 120 dev_has_access 2

step "push the example image to Harbor as the developer"
# A Harbor user who signs in with Keycloak uses the CLI secret from the
# Harbor profile as password for the container tools.
cli_secret="$("$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
  "login=${HARBOR_URL}/c/oidc/login?redirect_url=%2Fharbor%2Fprojects" "get=${HARBOR_URL}/api/v2.0/users/current" \
  | jq -s -r '.[1].body.oidc_user_meta.secret')"
[[ -n "$cli_secret" && "$cli_secret" != null ]] || die "dev has no Harbor CLI secret"
podman build --quiet --tag localhost/hello:1.0.0 "$ROOT/examples/hello" >/dev/null
podman login --tls-verify=false --username dev --password-stdin localhost:30002 <<<"$cli_secret" >/dev/null
podman push --quiet --tls-verify=false localhost/hello:1.0.0 "localhost:30002/$TENANT/hello:1.0.0"
podman logout localhost:30002 >/dev/null
log "dev pushed the image with the rights of the developer group"

step "deploy revision 1"
api POST "/tenants/$TENANT/deployments" "$(jq -n \
  '{environment: "dev", name: "hello", package: "web-app", values: {image: "hello:1.0.0", replicas: 1, health_path: "/healthz", env: {GREETING: "Hello from revision 1"}}}')" >/dev/null
wait_for "revision 1 is deployed" 300 deployment_is deployed 1
expect_greeting "Hello from revision 1"
api GET "/tenants/$TENANT/deployments/dev/hello" | jq -e '.revisions[0].images.image | test("@sha256:")' >/dev/null
# The guard compares the application and the project of the tenant with
# what Squared puts into them.
wait_for "the guard has no errors with the application and the project of the tenant" 60 guard_clean

step "check the Argo CD rights of the developer"
out="$("$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
  "login=http://localhost:30081/auth/login?return_url=http%3A%2F%2Flocalhost%3A30081%2Fapplications" \
  "get=http://localhost:30081/api/v1/applications?projects=tenant-$TENANT" \
  "get=http://localhost:30081/api/v1/account/can-i/applications/get/tenant-$TENANT/*" \
  "get=http://localhost:30081/api/v1/account/can-i/applications/sync/tenant-$TENANT/*" \
  "get=http://localhost:30081/api/v1/applications/$TENANT-dev-hello/resource-tree?appNamespace=argocd")"
jq -s -e --arg app "$TENANT-dev-hello" '[.[1].body.items[]?.metadata.name] | index($app) != null' <<<"$out" >/dev/null \
  || die "dev does not see the application in Argo CD"
[[ "$(jq -s -r '"\(.[2].body.value) \(.[3].body.value)"' <<<"$out")" == "yes no" ]] \
  || die "dev has the wrong rights in Argo CD: $(jq -s -c '[.[2].body.value, .[3].body.value]' <<<"$out")"
# Argo CD shows only the kinds that the project permits in the tree, and
# gives the logs only of a pod in the tree.
jq -s -e '[.[4].body.nodes[]? | select(.kind == "Pod")] | length > 0' <<<"$out" >/dev/null \
  || die "dev does not see the pods of the application in Argo CD"
log "dev can read the applications of the tenant in Argo CD, but not sync them"

step "use the Argo CD web interface as administrator"
# The API server shows the live objects, the events and the logs, and runs
# resource actions, with the limited rights of deploy/kind/argocd-values.yaml.
argo_api="http://localhost:30081/api/v1"
argo_token="$(curl -fsS "$argo_api/session" -H 'Content-Type: application/json' -d "$(jq -n \
  --arg p "$(k -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)" \
  '{username: "admin", password: $p}')" | jq -er .token)" || die "cannot sign in to Argo CD as admin"
argo_call() { curl -fsS -H "Authorization: Bearer $argo_token" "$@"; }
app="$TENANT-dev-hello"
res="namespace=$NS&resourceName=hello-web&version=v1&kind=Deployment&group=apps&appNamespace=argocd"
argo_call "$argo_api/applications/$app/resource?$res" | jq -e '.manifest | fromjson | .metadata.name == "hello-web"' >/dev/null \
  || die "Argo CD does not show the Deployment"
argo_call "$argo_api/applications/$app/resource-tree?appNamespace=argocd" | jq -e '[.nodes[] | select(.kind == "Pod")] | length > 0' >/dev/null \
  || die "Argo CD does not show the pods of the application"
uid="$(k -n "$NS" get deployment hello-web -o jsonpath='{.metadata.uid}')"
argo_call "$argo_api/applications/$app/events?resourceNamespace=$NS&resourceName=hello-web&resourceUID=$uid&appNamespace=argocd" \
  | jq -e 'has("items")' >/dev/null || die "Argo CD does not show the events of the Deployment"
pod="$(k -n "$NS" get pods -o jsonpath='{.items[0].metadata.name}')"
ctr="$(k -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].name}')"
argo_call "$argo_api/applications/$app/logs?namespace=$NS&podName=$pod&container=$ctr&tailLines=5&follow=false&appNamespace=argocd" \
  | grep -q '"result"' || die "Argo CD does not show the logs of the pod"
argo_call -X POST -H 'Content-Type: application/json' "$argo_api/applications/$app/resource/actions?$res" -d '"restart"' >/dev/null \
  || die "Argo CD cannot run the action restart"
restarted() { [[ -n "$(k -n "$NS" get deployment hello-web -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}')" ]]; }
wait_for "the action restart changes the Deployment" 60 restarted
[[ "$(k auth can-i get secrets -n kube-system --as system:serviceaccount:argocd:argocd-server)" == no ]] \
  || die "the API server of Argo CD can read the Secrets of kube-system"
wait_for "revision 1 is deployed after the restart" 300 deployment_is deployed 1
log "the administrator sees the objects, the events and the logs in Argo CD, and runs actions"

step "check tenant isolation"
# A pod in another namespace tries to connect. The network policy of the
# tenant must block it, thus curl fails and the pod phase is "Failed".
k create namespace "${TENANT}-outside" >/dev/null
k -n "${TENANT}-outside" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: probe
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: probe
      image: docker.io/curlimages/curl:8.16.0
      args: ["-sS", "--max-time", "5", "http://hello-web.${NS}.svc.cluster.local/"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
YAML
wait_for "probe pod has finished" 180 probe_done
[[ "$(probe_phase)" == "Failed" ]] || die "a pod in another namespace can connect to the tenant"
k delete namespace "${TENANT}-outside" --wait=false >/dev/null
log "pods of other namespaces cannot connect"

step "deploy revision 2 with a changed value"
api PUT "/tenants/$TENANT/deployments/dev/hello" "$(jq -n \
  '{values: {image: "hello:1.0.0", replicas: 1, health_path: "/healthz", env: {GREETING: "Hello from revision 2"}}, message: "e2e change"}')" >/dev/null
wait_for "revision 2 is deployed" 300 deployment_is deployed 2
expect_greeting "Hello from revision 2"

step "roll back to revision 1"
api POST "/tenants/$TENANT/deployments/dev/hello/rollback" '{"revision": 1}' >/dev/null
wait_for "revision 3 (copy of 1) is deployed" 300 deployment_is deployed 3
expect_greeting "Hello from revision 1"

step "remove the developer from the tenant"
dev_id="$(api GET "/tenants/$TENANT/members" | jq -r '.items[] | select(.email == "dev@squared.test") | .user_id')"
api DELETE "/tenants/$TENANT/members/$dev_id" >/dev/null
dev_lost_access() { [[ "$(dev_access)" != 200* && "$(dev_access)" == *" 404" ]]; }
wait_for "dev loses the Harbor project and the Forgejo repository" 120 dev_lost_access

step "remove the deployment"
api DELETE "/tenants/$TENANT/deployments/dev/hello" >/dev/null
wait_for "deployment is removed" 300 deployment_gone
wait_for "objects are removed from the cluster" 120 bash -c "! kubectl --context $CONTEXT -n $NS get deployment hello-web >/dev/null 2>&1"

step "audit log"
api GET "/tenants/$TENANT/audit" | jq -r '.items[] | "\(.time) \(.actor) \(.action) \(.target)"'

if [[ "${KEEP_TENANT:-}" == "1" ]]; then
  printf '\n\033[1;32mThe end-to-end test passed.\033[0m Tenant %s stays for inspection.\n' "$TENANT"
  exit 0
fi

step "remove the tenant"
api DELETE "/tenants/$TENANT" "$(jq -n --arg s "$TENANT" '{confirm: $s}')" >/dev/null
tenant_gone() {
  [[ "$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "${SQUARED_URL}/api/v1/tenants/$TENANT")" == "404" ]]
}
wait_for "tenant is removed" 120 tenant_gone
wait_for "Argo CD project is removed" 120 bash -c "! kubectl --context $CONTEXT -n argocd get appproject tenant-$TENANT >/dev/null 2>&1"
# The namespace stays by design. Remove it, as an administrator does.
k delete namespace "$NS" --wait=false >/dev/null

printf '\n\033[1;32mThe end-to-end test passed.\033[0m Run with KEEP_TENANT=1 to keep the tenant for inspection.\n'
