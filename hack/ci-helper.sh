#!/usr/bin/env bash

set -xeEo pipefail

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

function install_s5cmd() {
  tmp=$(mktemp -d)
  curl --fail -sSL -o "$tmp"/s5cmd.tar.gz https://github.com/peak/s5cmd/releases/download/v2.0.0/s5cmd_2.0.0_Linux-64bit.tar.gz
  tar xf "$tmp"/s5cmd.tar.gz -C "$tmp"
  sudo install "$tmp"/s5cmd /usr/local/bin/s5cmd
  rm -rf "$tmp"
}

function get_s3_creds() {
  kubectl -n rook-s3-nano-system exec deploy/rgw-objectstore-sample-rook-s3-nano-system -- radosgw-admin-sqlite user create --uid=ci --display-name="ci demo user" --access-key=foo --secret-key=bar
  export AWS_ACCESS_KEY_ID=foo
  export AWS_SECRET_ACCESS_KEY=bar
}

function run_s3_ops() {
  install_s5cmd
  ip=$(kubectl -n rook-s3-nano-system get svc rgw-objectstore-sample-rook-s3-nano-system -o jsonpath='{.spec.clusterIP}')
  get_s3_creds
  curl http://"$ip":8080
  echo -n
  s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 mb s3://foo
  s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 sync . s3://foo
  s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 ls s3://foo
}

FUNCTION="$1"
shift # remove function arg now that we've recorded it
# call the function with the remainder of the user-provided args
# -e, -E, and -o=pipefail will ensure this script returns a failure if a part of the function fails
$FUNCTION "$@"
