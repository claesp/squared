#!/usr/bin/env bash
# Builds the Squared image, loads it into the Kind cluster and deploys it.
# Use it after a code change. hack/kind-up.sh must have run one time.
source "$(dirname "$0")/lib.sh"

need kind kubectl podman

version="$(git -C "$ROOT" describe --always --dirty 2>/dev/null || echo dev)"
log "building image localhost/squared:dev ($version)"
podman build --quiet --tag localhost/squared:dev --build-arg "VERSION=$version" "$ROOT" >/dev/null

log "loading image into the cluster"
mkdir -p "$DEV"
podman save --quiet --output "$DEV/squared-image.tar" localhost/squared:dev
kind load image-archive "$DEV/squared-image.tar" --name "$CLUSTER" >/dev/null
rm -f "$DEV/squared-image.tar"

log "applying manifests"
k apply -k "$ROOT/deploy/kubernetes/kind" >/dev/null
# The image tag does not change, thus restart to use the new image.
k -n squared rollout restart deployment/squared-server deployment/squared-worker >/dev/null
k -n squared-guard rollout restart deployment/squared-guard >/dev/null
k -n squared rollout status statefulset/postgres --timeout 5m >/dev/null
k -n squared rollout status statefulset/rabbitmq --timeout 5m >/dev/null
k -n squared rollout status deployment/squared-server --timeout 5m >/dev/null
k -n squared rollout status deployment/squared-worker --timeout 5m >/dev/null
k -n squared-guard rollout status deployment/squared-guard --timeout 5m >/dev/null
# Wait until the old pods are gone. Until then, the service can still send
# requests to an old pod that is stopping.
for name in squared-server squared-worker; do
  want="$(k -n squared get deployment "$name" -o jsonpath='{.spec.replicas}')"
  until [[ "$(k -n squared get pods -l "app.kubernetes.io/name=$name" --no-headers 2>/dev/null | wc -l)" -eq "$want" ]]; do
    sleep 2
  done
done
log "Squared $version is running at $SQUARED_URL"
