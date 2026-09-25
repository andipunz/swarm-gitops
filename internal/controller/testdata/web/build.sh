#!/bin/sh
# Builds localtest/web:1 and :2 for the integration test (FROM scratch, no registry needed).
set -e
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -o web .
printf 'FROM scratch\nCOPY web /web\nENTRYPOINT ["/web"]\n' | docker build -q -t localtest/web:1 -f - .
printf 'FROM scratch\nCOPY web /web\nENV VERSION=2\nENTRYPOINT ["/web"]\n' | docker build -q -t localtest/web:2 -f - .
rm -f web
