#!/usr/bin/env bash
# Scans the full git history for secrets with gitleaks from its pinned image (repo mounted
# read-only). Needs docker. In CI the checkout must be complete (actions/checkout fetch-depth: 0).
set -euo pipefail

cd "$(dirname "$0")/.."
. ./deploy/local/versions.env

config=()
[ -f .gitleaks.toml ] && config=(--config /repo/.gitleaks.toml)

if docker run --rm -v "$PWD:/repo:ro" "$GITLEAKS_IMAGE" git /repo \
  --redact --no-banner --verbose ${config[@]+"${config[@]}"}; then
  echo "SECRETS OK"
else
  echo "FAIL: gitleaks reported findings (or failed to run); see output above" >&2
  exit 1
fi
