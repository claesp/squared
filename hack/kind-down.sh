#!/usr/bin/env bash
# Removes the Kind cluster and the tokens that belong to it. The passwords
# in .dev/ stay, thus a new cluster gets the same passwords.
source "$(dirname "$0")/lib.sh"

need kind
kind delete cluster --name "$CLUSTER"
rm -f "$DEV"/forgejo-*-token
