#!/bin/sh
# Builds the images for the integration tests (FROM scratch, no registry needed):
#   localtest/web:1, localtest/web:2   test web service
#   localtest/swarm-gitops:test        controller binary for the per-node prepare job
set -e
DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$DIR/../../../.." && pwd)

cd "$DIR"
CGO_ENABLED=0 go build -o web .
printf 'FROM scratch\nCOPY web /web\nENTRYPOINT ["/web"]\n' | docker build -q -t localtest/web:1 -f - .
printf 'FROM scratch\nCOPY web /web\nENV VERSION=2\nENTRYPOINT ["/web"]\n' | docker build -q -t localtest/web:2 -f - .
rm -f web

cd "$REPO"
CGO_ENABLED=0 go build -o "$DIR/swarm-gitops" ./cmd/swarm-gitops
cd "$DIR"
printf 'FROM scratch\nCOPY swarm-gitops /swarm-gitops\nENTRYPOINT ["/swarm-gitops"]\n' | docker build -q -t localtest/swarm-gitops:test -f - .
rm -f swarm-gitops
