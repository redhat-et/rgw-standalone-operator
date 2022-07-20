#!/usr/bin/env bash

set -xeEo pipefail

function install_deps() {
  sudo wget https://github.com/mikefarah/yq/releases/download/3.4.1/yq_linux_amd64 -O /usr/local/bin/yq
  sudo chmod +x /usr/local/bin/yq
}

function wait_for_rgw() {
  ns=$1
  for _ in {1..120}; do
    if [ "$(kubectl -n "$ns" get pod -l object_store=objectstore-sample --no-headers --field-selector=status.phase=Running|wc -l)" -ge 1 ] ; then
        echo "found rgw pod"
        break
    fi
    echo "waiting for rgw pods"
    sleep 5
  done
}

function install_s5cmd() {
  if ! command -v s5cmd; then
    tmp=$(mktemp -d)
    curl --fail -sSL -o "$tmp"/s5cmd.tar.gz https://github.com/peak/s5cmd/releases/download/v2.0.0/s5cmd_2.0.0_Linux-64bit.tar.gz
    tar xf "$tmp"/s5cmd.tar.gz -C "$tmp"
    sudo install "$tmp"/s5cmd /usr/local/bin/s5cmd
    rm -rf "$tmp"
  fi
}

function get_s3_creds() {
  ns=$1
  kubectl -n "$1" exec deploy/rgw-objectstore-sample-"$ns" -- radosgw-admin-sqlite user create --uid="$ns" --display-name="ci demo user" --access-key=foo --secret-key=bar

}

function run_s3_ops() {
  ns=$1
  action=$2
  install_s5cmd
  ip=$(kubectl -n "$ns" get svc rgw-objectstore-sample-"$ns" -o jsonpath='{.spec.clusterIP}')
  export AWS_ACCESS_KEY_ID=foo
  export AWS_SECRET_ACCESS_KEY=bar
  curl http://"$ip":8080
  echo -n
  if [[ "$action" == "write" ]]; then
    get_s3_creds "$ns"
    s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 mb s3://foo
    s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 sync . s3://foo
    s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 ls s3://foo
  elif [[ "$action" == "read" ]]; then
    s5cmd --no-verify-ssl --endpoint-url http://"$ip":8080 ls s3://foo
  else
    echo "unknown action: $action"
    exit 1
  fi
}

FUNCTION="$1"
shift # remove function arg now that we've recorded it
# call the function with the remainder of the user-provided args
# -e, -E, and -o=pipefail will ensure this script returns a failure if a part of the function fails
$FUNCTION "$@"
