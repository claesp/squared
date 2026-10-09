#!/usr/bin/env bash
# End-to-end test against the Kind development environment.
#
# The test signs in to the web interface, makes an API token, makes a
# tenant, pushes an image to Harbor, deploys the image, changes it, rolls
# it back and removes it. It also checks that pods of other namespaces
# cannot connect to the tenant. At the end it removes the tenant. Set
# KEEP_TENANT=1 to keep the tenant for inspection. Run hack/kind-up.sh
# first.
source "$(dirname "$0")/lib.sh"

need curl jq podman kubectl

TENANT="e2e$(date +%s | tail -c 6)"
NS="${TENANT}-dev"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
ADMIN_EMAIL="admin@squared.test"
ADMIN_PASSWORD="$(secret squared-admin-password)"

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

step "sign in to the web interface and make an API token"
JAR="$WORK/cookies"
curl -sS -c "$JAR" -b "$JAR" -o /dev/null --data-urlencode "email=$ADMIN_EMAIL" \
  --data-urlencode "password=$ADMIN_PASSWORD" "${SQUARED_URL}/login"
CSRF="$(curl -sS -c "$JAR" -b "$JAR" "${SQUARED_URL}/account" | sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' | head -1)"
[[ -n "$CSRF" ]] || die "sign-in failed"
TOKEN="$(curl -sS -c "$JAR" -b "$JAR" --data-urlencode "csrf=$CSRF" --data-urlencode "name=e2e $TENANT" \
  --data-urlencode "days=1" "${SQUARED_URL}/account/tokens" | sed -n 's/.*<pre class="token">\([^<]*\)<.*/\1/p')"
[[ "$TOKEN" == sqp_* ]] || die "no API token"
api GET /me | jq -e '.admin == true' >/dev/null

step "make tenant $TENANT"
api POST /tenants "$(jq -n --arg s "$TENANT" --arg o "$ADMIN_EMAIL" \
  '{slug: $s, name: ("End-to-end " + $s), owner: $o, environments: ["dev"], quota: {cpu: "2", memory: "2Gi", pods: 10}}')" >/dev/null
wait_for "tenant is ready" 300 tenant_ready
k get namespace "$NS" >/dev/null
k -n "$NS" get secret squared-registry >/dev/null

step "push the example image to Harbor"
podman build --quiet --tag localhost/hello:1.0.0 "$ROOT/examples/hello" >/dev/null
secret harbor-admin-password | podman login --tls-verify=false --username admin --password-stdin localhost:30002 >/dev/null
podman push --quiet --tls-verify=false localhost/hello:1.0.0 "localhost:30002/$TENANT/hello:1.0.0"

step "deploy revision 1"
api POST "/tenants/$TENANT/deployments" "$(jq -n \
  '{environment: "dev", name: "hello", package: "web-app", values: {image: "hello:1.0.0", replicas: 1, health_path: "/healthz", env: {GREETING: "Hello from revision 1"}}}')" >/dev/null
wait_for "revision 1 is deployed" 300 deployment_is deployed 1
expect_greeting "Hello from revision 1"
api GET "/tenants/$TENANT/deployments/dev/hello" | jq -e '.revisions[0].images.image | test("@sha256:")' >/dev/null

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
