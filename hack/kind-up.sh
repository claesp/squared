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
  log "installing the Forgejo database"
  k -n forgejo create secret generic forgejo-db --from-literal="password=$(secret forgejo-db-password)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -f "$ROOT/deploy/kind/forgejo-db.yaml" -f "$ROOT/deploy/kind/forgejo-config.yaml" >/dev/null
  k -n forgejo rollout status statefulset/forgejo-db --timeout 5m >/dev/null
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
  # The worker makes the accounts of tenant members for the authentication
  # source keycloak, by its ID. Forgejo has no API for the sources.
  local source_id want
  source_id="$(k -n forgejo exec deploy/forgejo -c forgejo -- gitea admin auth list | awk '$2 == "keycloak" {print $1}')"
  want="$(sed -n 's/.*SQUARED_FORGEJO_AUTH_SOURCE_ID=\([0-9]*\).*/\1/p' "$ROOT/deploy/kubernetes/kind/kustomization.yaml")"
  [[ -n "$source_id" && "$source_id" == "$want" ]] \
    || die "the Forgejo authentication source keycloak has the ID '$source_id', but SQUARED_FORGEJO_AUTH_SOURCE_ID is '$want'"
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
    --from-literal="squared-guard-client-secret=$(secret keycloak-squared-guard-client-secret)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -k "$ROOT/deploy/kind/keycloak" >/dev/null
  k -n keycloak rollout status deployment/keycloak --timeout 10m >/dev/null
  wait_http "${KEYCLOAK_URL}/realms/squared/.well-known/openid-configuration"
}

# setup_forgejo_guard gives the guard read access to the group settings
# of the Forgejo authentication sources: a view without the client secrets,
# and a role that can read only that view.
setup_forgejo_guard() {
  log "giving the guard read access to the Forgejo authentication sources"
  k -n forgejo exec -i statefulset/forgejo-db -- psql -q -U forgejo -d forgejo -v ON_ERROR_STOP=1 \
    < "$ROOT/deploy/kind/forgejo-guard-view.sql" >/dev/null
  k -n forgejo exec -i statefulset/forgejo-db -- psql -q -U forgejo -d forgejo -v ON_ERROR_STOP=1 \
    -v "pw=$(secret forgejo-db-guard-password)" >/dev/null <<'SQL'
ALTER ROLE squared_guard PASSWORD :'pw';
SQL
}

# setup_harbor makes Keycloak the authentication of Harbor. The settings
# come from the ConfigMap harbor-oidc. Harbor
# permits the change of the authentication mode only while it has no users
# other than "admin". The local "admin" account stays for the worker and
# for emergencies.
setup_harbor() {
  wait_http "${HARBOR_URL}/api/v2.0/ping"
  log "configuring Harbor sign-in with Keycloak"
  k apply -f "$ROOT/deploy/kind/harbor-oidc.yaml" >/dev/null
  local settings mode
  settings="$(k -n harbor get configmap harbor-oidc -o json | jq --arg secret "$(secret keycloak-harbor-client-secret)" \
    '.data + {oidc_client_secret: $secret, oidc_auto_onboard: true, oidc_verify_cert: false}')"
  mode="$(curl -fsS -u "admin:$(secret harbor-admin-password)" "${HARBOR_URL}/api/v2.0/configurations" | jq -r .auth_mode.value)"
  if [[ "$mode" != "oidc_auth" ]]; then
    settings="$(jq '. + {auth_mode: "oidc_auth"}' <<<"$settings")"
  fi
  curl -fsS -u "admin:$(secret harbor-admin-password)" -X PUT -H 'Content-Type: application/json' \
    -H 'X-Requested-With: squared' "${HARBOR_URL}/api/v2.0/configurations" -d "$settings" >/dev/null \
    || die "cannot configure Harbor sign-in"

  # A robot account with which the guard reads the members of all projects.
  local robot id
  robot="$(curl -fsS -u "admin:$(secret harbor-admin-password)" "${HARBOR_URL}/api/v2.0/robots?page_size=100" \
    | jq -r '.[] | select(.name == "robot$squared-guard") | .id' | head -1)"
  if [[ -z "$robot" ]]; then
    log "making the Harbor robot account of the guard"
    curl -fsS -u "admin:$(secret harbor-admin-password)" -X POST -H 'Content-Type: application/json' -H 'X-Requested-With: squared' \
      "${HARBOR_URL}/api/v2.0/robots" -d "$(jq -n --arg s "$(secret harbor-guard-robot-password)" '{
        name: "squared-guard", description: "Squared guard: reads project members.", level: "system", duration: -1, secret: $s,
        permissions: [
          {kind: "system", namespace: "/", access: [{resource: "project", action: "list"}]},
          {kind: "project", namespace: "*", access: [{resource: "member", action: "list"}]}]}')" >/dev/null \
      || die "cannot make the Harbor robot account of the guard"
    robot="$(curl -fsS -u "admin:$(secret harbor-admin-password)" "${HARBOR_URL}/api/v2.0/robots?page_size=100" \
      | jq -r '.[] | select(.name == "robot$squared-guard") | .id' | head -1)"
  fi
  # Harbor makes its own secret for a new robot account. Set the known one.
  curl -fsS -u "admin:$(secret harbor-admin-password)" -X PATCH -H 'Content-Type: application/json' -H 'X-Requested-With: squared' \
    "${HARBOR_URL}/api/v2.0/robots/$robot" -d "$(jq -n --arg s "$(secret harbor-guard-robot-password)" '{secret: $s}')" >/dev/null \
    || die "cannot set the secret of the Harbor robot account of the guard"
}

# setup_harbor_guard gives the guard read access to the group settings of
# the Harbor configuration: a view without the other values, and a role
# that can read only that view. The Harbor API gives the configuration
# only to system administrators.
setup_harbor_guard() {
  log "giving the guard read access to the Harbor configuration"
  k apply -f "$ROOT/deploy/kind/harbor-db-policy.yaml" >/dev/null
  k -n harbor exec -i statefulset/harbor-database -c database -- psql -q -U postgres -d registry -v ON_ERROR_STOP=1 \
    < "$ROOT/deploy/kind/harbor-guard-view.sql" >/dev/null
  k -n harbor exec -i statefulset/harbor-database -c database -- psql -q -U postgres -d registry -v ON_ERROR_STOP=1 \
    -v "pw=$(secret harbor-db-guard-password)" >/dev/null <<'SQL'
ALTER ROLE squared_guard PASSWORD :'pw';
SQL
}

# setup_keycloak_email lets only administrators set the email addresses.
# Squared links a new sign-in to an existing account with the same
# address, and the worker finds tenant members by it, thus a user must not
# choose an address. The guard checks
# this and the other email settings of the realm.
setup_keycloak_email() {
  log "letting only administrators change email addresses in Keycloak"
  local profile
  profile="$(kc GET /users/profile | jq '.attributes |= map(if .name == "email" then .permissions.edit = ["admin"] else . end)')"
  kc PUT /users/profile "$profile" >/dev/null
}

# Groups that are protected from the start. After the start, the Squared
# guard keeps the list up to date: it protects each group that a service
# configuration gives rights to.
PROTECTED_GROUPS=(squared-admins)

# kc METHOD PATH [JSON] calls the Keycloak admin API of the realm squared as
# the administrator of the master realm.
kc() {
  local token
  token="$(curl -fsS "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" -d grant_type=password \
    -d client_id=admin-cli -d username=admin --data-urlencode "password=$(secret keycloak-admin-password)" | jq -r .access_token)"
  curl -fsS -X "$1" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    "${KEYCLOAK_URL}/admin/realms/squared$2" ${3:+-d "$3"}
}

# kc_put_authz KIND NAME JSON makes or updates a policy (KIND
# "policy/user", "policy/role") or a permission (KIND "permission/scope")
# of the admin-permissions client.
kc_put_authz() {
  local kind="$1" name="$2" body="$3" base id
  base="/clients/$(kc GET '/clients?clientId=admin-permissions' | jq -r '.[0].id')/authz/resource-server"
  id="$(kc GET "$base/${kind%%/*}?name=$(jq -rn --arg n "$name" '$n|@uri')&exact=true" 2>/dev/null | jq -r --arg n "$name" '.[]? | select(.name == $n) | .id')"
  if [[ -n "$id" ]]; then
    kc PUT "$base/$kind/$id" "$(jq --arg id "$id" '. + {id: $id}' <<<"$body")" >/dev/null
  else
    kc POST "$base/$kind" "$body" >/dev/null
  fi
}

# kc_delete_authz KIND NAME removes a policy or permission if it exists.
kc_delete_authz() {
  local base id
  base="/clients/$(kc GET '/clients?clientId=admin-permissions' | jq -r '.[0].id')/authz/resource-server"
  id="$(kc GET "$base/$1?name=$(jq -rn --arg n "$2" '$n|@uri')" | jq -r --arg n "$2" '.[]? | select(.name == $n) | .id')"
  [[ -z "$id" ]] || kc DELETE "$base/$1/$id" >/dev/null
}

# setup_keycloak_permissions limits the service account of the Squared
# worker with fine-grained admin permissions (version 2). The account has
# no admin role other than query-users and query-groups. The permissions:
#   - Users (all): view, manage-group-membership.
#   - Groups (all): view, view-members, manage, manage-membership (for the
#     worker and the guard, see below).
#   - Protected groups: a permission for exactly these groups replaces the
#     permission for all groups for the scopes view, manage and
#     manage-membership. Its only policy is the realm role
#     squared-protected-group-admin, which nobody has. Thus the worker can
#     neither see nor change these groups, nor add or remove members.
#     The permission does not have the member scopes (view-members and so
#     on): they apply to the members themselves, and the worker must be
#     able to add an administrator to a tenant group.
# To add a user to a group, Keycloak needs manage-group-membership on the
# user and manage-membership on the group. Thus the worker cannot add a
# user to a protected group.
setup_keycloak_permissions() {
  log "limiting the Keycloak account of the Squared worker"
  local worker_client worker_user role_id groups=() g gid
  worker_client="$(kc GET '/clients?clientId=squared-worker' | jq -r '.[0].id')"
  worker_user="$(kc GET "/clients/$worker_client/service-account-user" | jq -r .id)"
  role_id="$(kc GET /roles/squared-protected-group-admin | jq -r .id)"
  for g in "${PROTECTED_GROUPS[@]}"; do
    gid="$(kc GET "/groups?search=$g&exact=true" | jq -r --arg n "$g" '.[] | select(.name == $n) | .id')"
    [[ -n "$gid" ]] || die "protected group $g does not exist"
    groups+=("$gid")
  done
  kc_put_authz policy/user squared-worker "$(jq -n --arg u "$worker_user" \
    '{name: "squared-worker", description: "The service account of the Squared worker.", logic: "POSITIVE", users: [$u]}')"
  kc_put_authz policy/role protected-group-administrators "$(jq -n --arg r "$role_id" \
    '{name: "protected-group-administrators", description: "Holders of the role squared-protected-group-admin.", logic: "POSITIVE", roles: [{id: $r, required: true}]}')"
  kc_put_authz permission/scope squared-worker-users "$(jq -n \
    '{name: "squared-worker-users", description: "The worker can find users and change their group memberships.",
      resourceType: "Users", scopes: ["view", "manage-group-membership"], policies: ["squared-worker"]}')"
  # The guard can make groups, so that a group that a configuration refers
  # to exists and is protected before somebody else makes it. It has
  # manage-realm (the only role that can change these permissions) and
  # view-users from its client roles.
  local guard_client guard_user
  guard_client="$(kc GET '/clients?clientId=squared-guard' | jq -r '.[0].id')"
  guard_user="$(kc GET "/clients/$guard_client/service-account-user" | jq -r .id)"
  kc_put_authz policy/user squared-guard "$(jq -n --arg u "$guard_user" \
    '{name: "squared-guard", description: "The service account of the Squared guard.", logic: "POSITIVE", users: [$u]}')"
  # Keycloak grants a scope only if all permissions for the same resources
  # grant it, and admin permissions accept only that strategy. Thus one
  # permission for all groups uses an aggregated policy, which is true for
  # the worker or the guard (AFFIRMATIVE). The guard has no
  # manage-group-membership on users, thus it cannot change memberships.
  kc_delete_authz permission squared-worker-groups
  kc_delete_authz permission squared-guard-groups
  # The guard sees all clients and client scopes with the role
  # view-clients, also the admin-permissions client. Keycloak does not
  # show it the client secrets. An older setup had a permission for that
  # one client.
  kc_delete_authz permission squared-guard-admin-permissions
  kc_put_authz policy/aggregate squared-worker-or-guard "$(jq -n \
    '{name: "squared-worker-or-guard", description: "The service account of the worker or of the guard.",
      logic: "POSITIVE", decisionStrategy: "AFFIRMATIVE", policies: ["squared-worker", "squared-guard"]}')"
  kc_put_authz permission/scope squared-groups "$(jq -n \
    '{name: "squared-groups", description: "The worker manages the tenant groups and their members. The guard makes the groups that it protects.",
      resourceType: "Groups", scopes: ["view", "view-members", "manage", "manage-membership"],
      policies: ["squared-worker-or-guard"]}')"
  # The guard updates the resources of this permission. Make it only one
  # time, so that a new run of this script does not remove the groups that
  # the guard added.
  if kc GET "/clients/$(kc GET '/clients?clientId=admin-permissions' | jq -r '.[0].id')/authz/resource-server/permission?name=protected-groups" | jq -e 'length > 0' >/dev/null; then
    return
  fi
  kc_put_authz permission/scope protected-groups "$(jq -n --args \
    '{name: "protected-groups", description: "Only holders of squared-protected-group-admin can manage these groups. This permission replaces squared-groups for them.",
      resourceType: "Groups", resources: $ARGS.positional,
      scopes: ["view", "manage", "manage-membership"],
      policies: ["protected-group-administrators"]}' "${groups[@]}")"
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
  k create namespace squared-guard --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n squared-guard create secret generic squared-guard \
    --from-literal="keycloak-client-secret=$(secret keycloak-squared-guard-client-secret)" \
    --from-literal="harbor-robot-password=$(secret harbor-guard-robot-password)" \
    --from-literal="forgejo-db-password=$(secret forgejo-db-guard-password)" \
    --from-literal="harbor-db-password=$(secret harbor-db-guard-password)" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  "$ROOT/hack/kind-deploy.sh"
}

create_cluster
configure_dns
deploy_keycloak
setup_keycloak_email
setup_keycloak_permissions
# The Forgejo pod has a sidecar with the Squared image.
build_squared_image
install_charts
setup_forgejo
setup_forgejo_guard
setup_harbor
setup_harbor_guard
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
