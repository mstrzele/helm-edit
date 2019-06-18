#!/usr/bin/env bash

# Copyright The Helm Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -euo pipefail

# Skip on pull request builds
if [[ -n "${CIRCLE_PR_NUMBER:-}" ]]; then
  exit
fi

: ${GCLOUD_SERVICE_KEY:?"GCLOUD_SERVICE_KEY environment variable is not set"}
: ${PROJECT_NAME:?"PROJECT_NAME environment variable is not set"}
: ${AZURE_STORAGE_CONNECTION_STRING:?"AZURE_STORAGE_CONNECTION_STRING environment variable is not set"}
: ${AZURE_STORAGE_CONTAINER_NAME:?"AZURE_STORAGE_CONTAINER_NAME environment variable is not set"}

VERSION=
if [[ -n "${CIRCLE_TAG:-}" ]]; then
  VERSION="${CIRCLE_TAG}"
elif [[ "${CIRCLE_BRANCH:-}" == "master" ]]; then
  VERSION="canary"
else
  echo "Skipping deploy step; this is neither master or a tag"
  exit
fi

echo "Install docker client"
VER="17.09.0-ce"
curl -L -o /tmp/docker-$VER.tgz https://download.docker.com/linux/static/stable/x86_64/docker-$VER.tgz
tar -xz -C /tmp -f /tmp/docker-$VER.tgz
mv /tmp/docker/* /usr/bin

echo "Install gcloud components"
export CLOUDSDK_CORE_DISABLE_PROMPTS=1
curl https://sdk.cloud.google.com | bash
${HOME}/google-cloud-sdk/bin/gcloud --quiet components update

echo "Configuring gcloud authentication"
echo "${GCLOUD_SERVICE_KEY}" | base64 --decode > "${HOME}/gcloud-service-key.json"
${HOME}/google-cloud-sdk/bin/gcloud auth activate-service-account --key-file "${HOME}/gcloud-service-key.json"
${HOME}/google-cloud-sdk/bin/gcloud config set project "${PROJECT_NAME}"
docker login -u _json_key -p "$(cat ${HOME}/gcloud-service-key.json)" https://gcr.io

echo "Installing Azure CLI"
apt update
apt install -y apt-transport-https
echo "deb [arch=amd64] https://packages.microsoft.com/repos/azure-cli/ stretch main" | tee /etc/apt/sources.list.d/azure-cli.list
curl -L https://packages.microsoft.com/keys/microsoft.asc | apt-key add
apt update
apt install -y azure-cli

echo "Building the tiller image"
make docker-build VERSION="${VERSION}"

echo "Pushing image to gcr.io"
docker push "gcr.io/kubernetes-helm/tiller:${VERSION}"

echo "Building helm binaries"
make build-cross
make dist checksum VERSION="${VERSION}"

echo "Pushing binaries to gs bucket"
${HOME}/google-cloud-sdk/bin/gsutil cp ./_dist/* "gs://${PROJECT_NAME}"

echo "Pushing binaries to Azure"
az storage blob upload-batch -s _dist/ -d "$AZURE_STORAGE_CONTAINER_NAME" --pattern 'helm-*' --connection-string "$AZURE_STORAGE_CONNECTION_STRING"
