#!/usr/bin/env bash
# Removes the Kind cluster and the tokens that belong to it. The passwords
# in .dev/ stay, thus a new cluster gets the same passwords.
source "$(dirname "$0")/lib.sh"

need kind
# The node has an anonymous volume for /var, with all images and the data
# of the cluster. Kind removes it with the node, but not when Podman
# reports the error below. Thus note it now and remove it at the end.
node="${CLUSTER}-control-plane"
volumes=()
if podman container exists "$node"; then
  mapfile -t volumes < <(podman inspect "$node" \
    --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' | grep -v '^$')
fi
# Rootless Podman can report an error about its network namespace after it
# removed the node. Thus check the result, not only the exit status.
if ! kind delete cluster --name "$CLUSTER"; then
  sleep 2
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || podman container exists "${CLUSTER}-control-plane"; then
    kind delete cluster --name "$CLUSTER"
  else
    log "the cluster is removed (Podman reported a cleanup error, which is not important)"
  fi
fi
for v in "${volumes[@]}"; do
  if podman volume exists "$v"; then
    podman volume rm "$v" >/dev/null
    log "removed the volume $v of the node"
  fi
done
rm -f "$DEV"/forgejo-*-token
