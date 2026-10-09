# Shared functions for the scripts in hack/. Source this file.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV="${ROOT}/.dev"
CLUSTER="${SQUARED_CLUSTER:-squared}"
CONTEXT="kind-${CLUSTER}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

# Pinned versions of the Helm charts. Change them here only.
ARGOCD_CHART_VERSION="10.10.2"
FORGEJO_CHART_VERSION="17.2.0"
HARBOR_CHART_VERSION="1.19.2"

FORGEJO_URL="http://localhost:30300"
HARBOR_URL="http://localhost:30002"
SQUARED_URL="http://localhost:30080"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

need() {
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || die "the tool '$tool' is necessary"
  done
}

k() { kubectl --context "$CONTEXT" "$@"; }

# secret NAME prints the value of a development secret. It makes a random
# value the first time.
secret() {
  local file="${DEV}/$1"
  if [[ ! -s "$file" ]]; then
    mkdir -p "$DEV"
    chmod 700 "$DEV"
    # 24 random bytes in base64 without characters that need quoting.
    # Harbor needs upper case, lower case and digits in passwords.
    printf 'Sq1%s' "$(openssl rand -base64 24 | tr -d '/+=\n')" > "$file"
    chmod 600 "$file"
  fi
  cat "$file"
}

# wait_http URL [SECONDS] waits until URL answers with a 2xx status.
wait_http() {
  local url="$1" timeout="${2:-300}" start
  start=$(date +%s)
  until curl -fsS -o /dev/null "$url" 2>/dev/null; do
    (( $(date +%s) - start > timeout )) && die "no answer from $url after ${timeout}s"
    sleep 3
  done
}

# kind_cmd runs kind. Rootless Podman needs a systemd scope with
# delegated cgroup controllers to create a cluster.
kind_cmd() {
  if [[ "$KIND_EXPERIMENTAL_PROVIDER" == "podman" ]] && \
     [[ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == "true" ]] && \
     command -v systemd-run >/dev/null 2>&1; then
    systemd-run --scope --quiet --user -p Delegate=yes kind "$@"
  else
    kind "$@"
  fi
}
