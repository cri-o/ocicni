#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

go fmt ./...
./hack/tree_status.sh
