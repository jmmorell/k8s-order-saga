#!/usr/bin/env bash
# Sets image.tag in the five service values files: bump-image-tag.sh <tag>, where <tag> is a
# 7-40 character lowercase hex commit SHA or 'dev'. Rewrites only the 'tag:' line inside the
# top-level 'image:' block, leaves every other byte alone and is idempotent. Every file is checked
# before any is written, so a failure modifies nothing. VALUES_DIR overrides the directory (tests).
# The tag is written quoted: an all-digit short SHA would otherwise be read as a YAML number.
set -euo pipefail

cd "$(dirname "$0")/.."

services="orders inventory payments notifications loadgen"
dir=${VALUES_DIR:-deploy/local/values}

fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$#" -eq 1 ] || fail "usage: $0 <tag> (7-40 lowercase hex characters, or dev)"
tag=$1
[ -n "$tag" ] || fail "the tag is empty"
[[ "$tag" =~ ^([0-9a-f]{7,40}|dev)$ ]] || fail "invalid tag '$tag': want 7-40 lowercase hex characters or dev"

# Line numbers of '  tag: ...' inside the top-level 'image:' block (which ends at the next line
# that starts with a non-space character).
tag_lines() {
  awk '/^image:[[:space:]]*$/ {f=1; next} f && /^[^[:space:]#]/ {f=0} f && /^  tag:/ {print NR}' "$1"
}

for svc in $services; do
  f=$dir/$svc.yaml
  [ -f "$f" ] || fail "$f does not exist"
  n=$(tag_lines "$f" | wc -l | tr -d ' ')
  [ "$n" -eq 1 ] || fail "$f: expected exactly one 'tag:' line under 'image:', found $n"
done

for svc in $services; do
  f=$dir/$svc.yaml
  line=$(tag_lines "$f")
  new="  tag: \"$tag\""
  [ "$(sed -n "${line}p" "$f")" = "$new" ] && continue
  tmp=$(mktemp)
  # awk rewrites only that line; every other line is printed as read.
  awk -v n="$line" -v new="$new" 'NR == n {print new; next} {print}' "$f" >"$tmp"
  # Write in place (keeps the file's mode); the file already ends in a newline, as awk's output does.
  cat "$tmp" >"$f"
  rm -f "$tmp"
done

echo "image.tag = \"$tag\" in $dir/{${services// /,}}.yaml"
