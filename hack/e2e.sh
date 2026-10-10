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

check_guard() {
  local st service group="e2e-ops-$TENANT" policy
  # All four services refer to the administrator group.
  guard_ran() { guard_status | jq -e '.protected | length > 0' >/dev/null 2>&1; }
  wait_for "the guard has run" 60 guard_ran
  st="$(guard_status)"
  for service in "Argo CD" Harbor Forgejo Squared; do
    jq -e --arg s "$service" '.protected[] | select(.path == "/squared-admins") | .references[] | select(.service == $s)' <<<"$st" >/dev/null \
      || die "the guard does not see that $service gives rights to /squared-admins: $(jq -c '[.protected[] | {path, services: [.references[].service]}], .errors' <<<"$st")"
  done
  # A new line in the Argo CD RBAC makes the group and protects it.
  policy="$(k -n argocd get configmap argocd-rbac-cm -o jsonpath='{.data.policy\.csv}')"
  k -n argocd patch configmap argocd-rbac-cm --type merge \
    -p "$(jq -n --arg p "$policy"$'\ng, /'"$group"$', role:readonly\n' '{data: {"policy.csv": $p}}')" >/dev/null
  guard_protected() { [[ "$(worker_may_add_dev "$group")" == 403 ]]; }
  wait_for "the guard made and protected /$group" 90 guard_protected
  # Without the line, the group is not protected anymore.
  k -n argocd patch configmap argocd-rbac-cm --type merge -p "$(jq -n --arg p "$policy" '{data: {"policy.csv": $p}}')" >/dev/null
  guard_released() { [[ "$(worker_may_add_dev "$group")" == 204 ]]; }
  wait_for "the guard released /$group" 90 guard_released
  local at gid
  at="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password -d client_id=admin-cli \
    -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  gid="$(curl -fsS -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/groups?search=$group&exact=true" | jq -r '.[0].id')"
  curl -fsS -o /dev/null -X DELETE -H "Authorization: Bearer $at" "${KEYCLOAK_URL}/admin/realms/squared/groups/$gid"
  log "the guard follows the Argo CD RBAC configuration"
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

step "make tenant $TENANT"
api POST /tenants "$(jq -n --arg s "$TENANT" --arg o "$ADMIN_EMAIL" \
  '{slug: $s, name: ("End-to-end " + $s), owner: $o, environments: ["dev"], quota: {cpu: "2", memory: "2Gi", pods: 10}}')" >/dev/null
wait_for "tenant is ready" 300 tenant_ready
k get namespace "$NS" >/dev/null
k -n "$NS" get secret squared-registry >/dev/null

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

step "check the Argo CD rights of the developer"
out="$("$SSO" -user dev -password-file "$DEV/keycloak-dev-user-password" \
  "login=http://localhost:30081/auth/login?return_url=http%3A%2F%2Flocalhost%3A30081%2Fapplications" \
  "get=http://localhost:30081/api/v1/applications?projects=tenant-$TENANT" \
  "get=http://localhost:30081/api/v1/account/can-i/applications/get/tenant-$TENANT/*" \
  "get=http://localhost:30081/api/v1/account/can-i/applications/sync/tenant-$TENANT/*")"
jq -s -e --arg app "$TENANT-dev-hello" '[.[1].body.items[]?.metadata.name] | index($app) != null' <<<"$out" >/dev/null \
  || die "dev does not see the application in Argo CD"
[[ "$(jq -s -r '"\(.[2].body.value) \(.[3].body.value)"' <<<"$out")" == "yes no" ]] \
  || die "dev has the wrong rights in Argo CD: $(jq -s -c '[.[2].body.value, .[3].body.value]' <<<"$out")"
log "dev can read the applications of the tenant in Argo CD, but not sync them"

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
