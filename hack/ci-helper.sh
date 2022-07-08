#!/usr/bin/env bash

set -xeEo pipefail

echo lol

function wait_for_rgw() {
  for _ in {1..120}; do
    if [ "$(kubectl -n rook-s3-nano-system get pod -l object_store=objectstore-sample --no-headers --field-selector=status.phase=Running|wc -l)" -ge 1 ] ; then
        echo "found rgw pod"
        break
    fi
    echo "waiting for rgw pods"
    sleep 5
  done
}

FUNCTION="$1"
shift # remove function arg now that we've recorded it
# call the function with the remainder of the user-provided args
# -e, -E, and -o=pipefail will ensure this script returns a failure if a part of the function fails
$FUNCTION "$@"
