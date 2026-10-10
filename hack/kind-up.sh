#!/usr/bin/env bash
# Makes the complete development environment in Kind:
#   Argo CD, Forgejo, Harbor, PostgreSQL, RabbitMQ and Squared.
# It is safe to run this script again. Secrets are in .dev/ (not in Git).
source "$(dirname "$0")/lib.sh"

need kind kubectl helm podman curl jq openssl

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    log "cluster $CLUSTER exists"
  else
    log "making cluster $CLUSTER"
    kind_cmd create cluster --name "$CLUSTER" --config "$ROOT/deploy/kind/cluster.yaml" --wait 120s
  fi
  # The node pulls images from Harbor at localhost:30002 over plain HTTP.
  podman exec "${CLUSTER}-control-plane" sh -c '
    mkdir -p "/etc/containerd/certs.d/localhost:30002"
    cat > "/etc/containerd/certs.d/localhost:30002/hosts.toml" <<TOML
server = "http://localhost:30002"

[host."http://localhost:30002"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
TOML'
}

# configure_dns makes the names keycloak.localhost and forgejo.localhost
# point to the services inside the cluster. The services have the same
# ports as their NodePorts, thus each address works inside and outside
# the cluster.
configure_dns() {
  local corefile block
  corefile="$(k -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
  if grep -q 'keycloak.localhost' <<<"$corefile"; then
    return
  fi
  log "adding keycloak.localhost and forgejo.localhost to CoreDNS"
  block='    rewrite stop {\n        name exact keycloak.localhost keycloak.keycloak.svc.cluster.local\n        answer name keycloak.keycloak.svc.cluster.local keycloak.localhost\n    }\n    rewrite stop {\n        name exact forgejo.localhost forgejo-http.forgejo.svc.cluster.local\n        answer name forgejo-http.forgejo.svc.cluster.local forgejo.localhost\n    }'
  corefile="$(sed "s|^    ready\$|    ready\n${block}|" <<<"$corefile")"
  k -n kube-system patch configmap coredns --type merge \
    --patch "$(jq -n --arg c "$corefile" '{data: {Corefile: $c}}')" >/dev/null
  k -n kube-system rollout restart deployment/coredns >/dev/null
  k -n kube-system rollout status deployment/coredns --timeout 2m >/dev/null
}

install_charts() {
  log "installing Argo CD $ARGOCD_CHART_VERSION"
  helm --kube-context "$CONTEXT" upgrade --install argocd argo-cd \
    --repo https://argoproj.github.io/argo-helm --version "$ARGOCD_CHART_VERSION" \
    --namespace argocd --create-namespace \
    --values "$ROOT/deploy/kind/argocd-values.yaml" \
    --set-string "configs.secret.extra.oidc\\.keycloak\\.clientSecret=$(secret keycloak-argocd-client-secret)" \
    --wait --timeout 10m >/dev/null

  k create namespace forgejo --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n forgejo create secret generic forgejo-oauth --from-literal=key=forgejo \
    --from-literal="secret=$(secret keycloak-forgejo-client-secret)" --dry-run=client -o yaml | k apply -f - >/dev/null
  log "installing Forgejo $FORGEJO_CHART_VERSION"
  helm --kube-context "$CONTEXT" upgrade --install forgejo oci://code.forgejo.org/forgejo-helm/forgejo \
    --version "$FORGEJO_CHART_VERSION" --namespace forgejo --create-namespace \
    --values "$ROOT/deploy/kind/forgejo-values.yaml" \
    --set-string "gitea.admin.password=$(secret forgejo-admin-password)" --wait --timeout 10m >/dev/null

  log "installing Harbor $HARBOR_CHART_VERSION (this takes some minutes)"
  helm --kube-context "$CONTEXT" upgrade --install harbor harbor \
    --repo https://helm.goharbor.io --version "$HARBOR_CHART_VERSION" \
    --namespace harbor --create-namespace \
    --values "$ROOT/deploy/kind/harbor-values.yaml" \
    --set-string "harborAdminPassword=$(secret harbor-admin-password)" --wait --timeout 15m >/dev/null
}

# fj METHOD PATH [JSON] calls the Forgejo API as the administrator.
fj() {
  curl -sS -u "forgejo-admin:$(secret forgejo-admin-password)" -X "$1" \
    -H 'Content-Type: application/json' "${FORGEJO_URL}/api/v1$2" ${3:+-d "$3"}
}

# fj_user USER METHOD PATH [JSON] calls the Forgejo API as a user with a
# password.
fj_user() {
  curl -sS -u "$1:$(secret "forgejo-$1-password")" -X "$2" \
    -H 'Content-Type: application/json' "${FORGEJO_URL}/api/v1$3" ${4:+-d "$4"}
}

ensure_forgejo_user() {
  local user="$1"
  if [[ "$(fj GET "/users/$user" | jq -r '.login // empty')" != "$user" ]]; then
    log "making Forgejo user $user"
    fj POST /admin/users "$(jq -n --arg u "$user" --arg p "$(secret "forgejo-$user-password")" \
      '{username: $u, email: ($u + "@local.invalid"), password: $p, must_change_password: false}')" >/dev/null
  fi
}

# ensure_forgejo_token USER NAME SCOPES writes .dev/forgejo-USER-token.
ensure_forgejo_token() {
  local user="$1" name="$2" scopes="$3" file="${DEV}/forgejo-$1-token"
  if [[ -s "$file" ]] && curl -fsS -o /dev/null -H "Authorization: token $(cat "$file")" "${FORGEJO_URL}/api/v1/user"; then
    return
  fi
  log "making Forgejo token for $user"
  fj_user "$user" DELETE "/users/$user/tokens/$name" >/dev/null || true
  fj_user "$user" POST "/users/$user/tokens" "$(jq -n --arg n "$name" --argjson s "$scopes" '{name: $n, scopes: $s}')" \
    | jq -er .sha1 > "$file" || die "cannot make the Forgejo token for $user"
  chmod 600 "$file"
}

setup_forgejo() {
  wait_http "${FORGEJO_URL}/api/healthz"
  ensure_forgejo_user squared-bot
  ensure_forgejo_user argocd-reader
  # The worker makes Forgejo accounts for tenant members, thus the bot is a
  # Forgejo administrator.
  fj PATCH /admin/users/squared-bot '{"login_name": "squared-bot", "source_id": 0, "admin": true}' >/dev/null
  ensure_forgejo_token squared-bot squared-worker '["write:organization","write:repository","write:admin","read:user"]'
  ensure_forgejo_token argocd-reader argocd '["read:repository"]'
  local bot
  bot="$(cat "${DEV}/forgejo-squared-bot-token")"
  botapi() { curl -sS -H "Authorization: token $bot" -H 'Content-Type: application/json' -X "$1" "${FORGEJO_URL}/api/v1$2" ${3:+-d "$3"}; }

  if [[ "$(botapi GET /orgs/squared | jq -r '.username // empty')" != "squared" ]]; then
    log "making Forgejo organization squared"
    botapi POST /orgs '{"username": "squared", "visibility": "private"}' >/dev/null
  fi
  local team
  team="$(botapi GET /orgs/squared/teams | jq -r '.[] | select(.name == "readers") | .id')"
  if [[ -z "$team" ]]; then
    log "making Forgejo team readers"
    team="$(botapi POST /orgs/squared/teams \
      '{"name": "readers", "permission": "read", "units": ["repo.code"], "includes_all_repositories": true}' | jq -r .id)"
  fi
  botapi PUT "/teams/$team/members/argocd-reader" >/dev/null
  local hook_url="http://argocd-server.argocd.svc.cluster.local/api/webhook"
  if ! botapi GET /orgs/squared/hooks | jq -e --arg u "$hook_url" '.[] | select(.config.url == $u)' >/dev/null; then
    log "making Forgejo webhook to Argo CD"
    botapi POST /orgs/squared/hooks "$(jq -n --arg u "$hook_url" \
      '{type: "gitea", active: true, events: ["push"], config: {url: $u, content_type: "json"}}')" >/dev/null
  fi
}

deploy_keycloak() {
  log "installing Keycloak"
  k create namespace keycloak --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n keycloak create secret generic keycloak \
    --from-literal="admin-password=$(secret keycloak-admin-password)" \
    --from-literal="squared-client-secret=$(secret keycloak-squared-client-secret)" \
    --from-literal="admin-user-password=$(secret keycloak-admin-user-password)" \
    --from-literal="dev-user-password=$(secret keycloak-dev-user-password)" \
    --from-literal="argocd-client-secret=$(secret keycloak-argocd-client-secret)" \
    --from-literal="harbor-client-secret=$(secret keycloak-harbor-client-secret)" \
    --from-literal="forgejo-client-secret=$(secret keycloak-forgejo-client-secret)" \
    --from-literal="squared-worker-client-secret=$(secret keycloak-squared-worker-client-secret)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -k "$ROOT/deploy/kind/keycloak" >/dev/null
  k -n keycloak rollout status deployment/keycloak --timeout 10m >/dev/null
  wait_http "${KEYCLOAK_URL}/realms/squared/.well-known/openid-configuration"
}

# setup_harbor makes Keycloak the authentication of Harbor. Harbor permits
# this change only while it has no users other than "admin". The local
# "admin" account stays for the worker and for emergencies. Members of the
# Keycloak group squared-admins are Harbor administrators.
setup_harbor() {
  wait_http "${HARBOR_URL}/api/v2.0/ping"
  local mode
  mode="$(curl -fsS -u "admin:$(secret harbor-admin-password)" "${HARBOR_URL}/api/v2.0/configurations" | jq -r .auth_mode.value)"
  if [[ "$mode" == "oidc_auth" ]]; then
    return
  fi
  log "configuring Harbor sign-in with Keycloak"
  curl -fsS -u "admin:$(secret harbor-admin-password)" -X PUT -H 'Content-Type: application/json' \
    -H 'X-Requested-With: squared' "${HARBOR_URL}/api/v2.0/configurations" -d "$(jq -n \
      --arg secret "$(secret keycloak-harbor-client-secret)" '{
        auth_mode: "oidc_auth",
        oidc_name: "Keycloak",
        oidc_endpoint: "http://keycloak.localhost:30180/realms/squared",
        oidc_client_id: "harbor",
        oidc_client_secret: $secret,
        oidc_scope: "openid,profile,email,offline_access",
        oidc_user_claim: "preferred_username",
        oidc_groups_claim: "groups",
        oidc_admin_group: "squared-admins",
        oidc_auto_onboard: true,
        oidc_verify_cert: false
      }')" >/dev/null || die "cannot configure Harbor sign-in"
}

setup_argocd() {
  log "configuring Argo CD"
  k -n argocd create secret generic squared-forgejo \
    --from-literal=type=git \
    --from-literal=url=http://forgejo.localhost:30300/squared \
    --from-literal=username=argocd-reader \
    --from-literal="password=$(cat "${DEV}/forgejo-argocd-reader-token")" \
    --dry-run=client -o yaml | k label --local -f - argocd.argoproj.io/secret-type=repo-creds -o yaml | k apply -f - >/dev/null
  k apply -f "$ROOT/deploy/kind/argocd-platform.yaml" >/dev/null
}

deploy_squared() {
  wait_http "${HARBOR_URL}/api/v2.0/ping"
  log "making Squared secrets"
  k create namespace squared --dry-run=client -o yaml | k apply -f - >/dev/null
  local db_url="postgres://squared:$(secret postgres-password)@postgres.squared.svc.cluster.local:5432/squared?sslmode=disable"
  local amqp_url="amqp://squared:$(secret rabbitmq-password)@rabbitmq.squared.svc.cluster.local:5672/"
  k -n squared create secret generic postgres --from-literal="password=$(secret postgres-password)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n squared create secret generic rabbitmq --from-literal="password=$(secret rabbitmq-password)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  # Each component gets only the secrets that it uses.
  k -n squared create secret generic squared-server \
    --from-literal="database-url=$db_url" --from-literal="amqp-url=$amqp_url" \
    --from-literal="oidc-client-secret=$(secret keycloak-squared-client-secret)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n squared create secret generic squared-worker \
    --from-literal="database-url=$db_url" --from-literal="amqp-url=$amqp_url" \
    --from-literal="forgejo-token=$(cat "${DEV}/forgejo-squared-bot-token")" \
    --from-literal="harbor-password=$(secret harbor-admin-password)" \
    --from-literal="keycloak-client-secret=$(secret keycloak-squared-worker-client-secret)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  "$ROOT/hack/kind-deploy.sh"
}

create_cluster
configure_dns
deploy_keycloak
install_charts
setup_forgejo
setup_harbor
setup_argocd
deploy_squared

cat <<INFO

The development environment is ready.

Squared, Argo CD, Harbor and Forgejo use Keycloak for sign-in:
  Squared   ${SQUARED_URL}
  Argo CD   http://localhost:30081
  Harbor    ${HARBOR_URL}
  Forgejo   ${FORGEJO_URL}

Keycloak users:
  platform-admin / $(secret keycloak-admin-user-password)   (member of squared-admins)
  dev / $(secret keycloak-dev-user-password)   (no administrator rights)

Local emergency accounts:
  Keycloak  ${KEYCLOAK_URL}   admin / $(secret keycloak-admin-password)   (administration console)
  Forgejo   forgejo-admin / $(secret forgejo-admin-password)
  Harbor    admin / $(secret harbor-admin-password)
  Argo CD   admin / $(k -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)

The passwords are also in ${DEV}/.
INFO
