#!/usr/bin/env bash
# Scans the full git history for secrets with gitleaks from its pinned image (repo mounted
# read-only). Needs docker. In CI the checkout must be complete (actions/checkout fetch-depth: 0):
# a shallow clone fails here, since gitleaks would scan the truncated history and still pass.
set -euo pipefail

cd "$(dirname "$0")/.."
. ./deploy/local/versions.env

fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$(git rev-parse --is-shallow-repository)" = false ] ||
  fail "shallow clone: gitleaks would scan only part of the history (fetch the full history, e.g. actions/checkout fetch-depth: 0)"

config=()
[ -f .gitleaks.toml ] && config=(--config /repo/.gitleaks.toml)

# On a CI runner the checkout belongs to another uid than the container user, and git refuses a
# repository it does not own ("dubious ownership"). safe.directory is passed as command-scope
# config, so it does not depend on the image's own gitconfig.
status=0
out=$(docker run --rm -v "$PWD:/repo:ro" \
  -e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0=/repo \
  "$GITLEAKS_IMAGE" git /repo --redact --no-banner --verbose ${config[@]+"${config[@]}"} 2>&1) || status=$?
echo "$out"
[ "$status" -eq 0 ] || fail "gitleaks reported findings (or failed to run); see output above"

# gitleaks exits 0 when git itself fails (it logs ERR and scans 0 commits): check it really read
# the history. (Its count leaves out merges and empty commits, so it is not compared exactly.)
plain=$(sed $'s/\x1b\\[[0-9;]*m//g' <<<"$out")
if grep -q ' ERR ' <<<"$plain"; then fail "gitleaks logged an error; see output above"; fi
scanned=$(sed -nE 's/.* ([0-9]+) commits scanned.*/\1/p' <<<"$plain" | tail -1)
[ -n "$scanned" ] && [ "$scanned" -gt 0 ] ||
  fail "gitleaks scanned no commits (expected up to $(git rev-list --all --no-merges --count))"

echo "SECRETS OK ($scanned commits)"
