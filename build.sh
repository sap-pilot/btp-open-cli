#!/usr/bin/env bash
set -euo pipefail

VERSION=$(git rev-parse --abbrev-ref HEAD)
COMMIT=$(git rev-parse --short=7 HEAD)
DATE=$(date -u +%Y-%m-%d)

echo "Building bo — version=${VERSION} commit=${COMMIT} date=${DATE}"

go build \
  -ldflags="-s -w \
    -X 'btp-open-cli/cmd.Version=${VERSION}' \
    -X 'btp-open-cli/cmd.Commit=${COMMIT}' \
    -X 'btp-open-cli/cmd.Date=${DATE}'" \
  -o bo .

echo "Done: ./bo"
