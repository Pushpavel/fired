#!/usr/bin/env bash
# Builds the image, pushes it and deploys the chart, like CD does on every
# merge to main. Needs docker, helm and skaffold.
#
#   REGISTRY=registry.example.com/team ./deploy.sh
#
# Uses .k8s/kubeconfig if present (else $KUBECONFIG / ~/.kube/config) and
# .k8s/values.yaml (your settings over fired/values.yaml). NAMESPACE
# defaults to "fired".
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

: "${REGISTRY:?set REGISTRY to the image repository, e.g. registry.example.com/team}"
if [ -f kubeconfig ]; then
  export KUBECONFIG="$PWD/kubeconfig"
fi
if [ ! -f values.yaml ]; then
  echo "values.yaml is missing: create it with your settings (token, postgres.password, artifacts.token, fired.groups); see fired/values.yaml" >&2
  exit 1
fi
for tool in docker helm skaffold; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
done
export NAMESPACE="${NAMESPACE:-fired}"
skaffold run -f skaffold.yaml --default-repo "$REGISTRY"
