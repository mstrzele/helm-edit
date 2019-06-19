#!/usr/bin/env bash

set -euf -o pipefail

HELM_EDIT_VERSION=${HELM_EDIT_VERSION:-"0.3.0"}

file="${HELM_PLUGIN_DIR:-"$(helm home)/plugins/helm-edit"}/helm-edit"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
url="https://github.com/mstrzele/helm-edit/releases/download/v${HELM_EDIT_VERSION}/helm-edit_${os}_amd64"

if command -v wget; then
  wget -O "${file}"  "${url}"
elif command -v curl; then
  curl -o "${file}" -L "${url}"
fi

chmod +x "${file}"
