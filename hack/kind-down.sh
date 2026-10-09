#!/usr/bin/env bash
# Removes the Kind cluster and the tokens that belong to it. The passwords
# in .dev/ stay, thus a new cluster gets the same passwords.
source "$(dirname "$0")/lib.sh"

need kind
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
rm -f "$DEV"/forgejo-*-token
