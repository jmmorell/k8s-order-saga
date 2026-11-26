#!/usr/bin/env bash
# Tests scripts/bump-image-tag.sh on copies of the five service values files: one changed line
# per file, idempotent, and a bad tag or a file without image.tag modifies nothing. Prints BUMP OK.
set -euo pipefail

cd "$(dirname "$0")/.."

services="orders inventory payments notifications loadgen"
bump=scripts/bump-image-tag.sh

fail() { echo "FAIL: $*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
orig=$tmp/orig
work=$tmp/work
mkdir "$orig" "$work"
for svc in $services; do
  cp "deploy/local/values/$svc.yaml" "$orig/"
  cp "deploy/local/values/$svc.yaml" "$work/"
done

run() { VALUES_DIR=$work "$bump" "$@"; }

# same_as <dir> — every work file is byte-identical to the file in <dir>.
same_as() {
  local svc
  for svc in $services; do
    cmp -s "$1/$svc.yaml" "$work/$svc.yaml" || return 1
  done
}

# A valid tag rewrites exactly the image.tag line of each file.
tag=0123abc
run "$tag" >/dev/null || fail "bump to $tag failed"
for svc in $services; do
  d=$(diff "$orig/$svc.yaml" "$work/$svc.yaml" || true)
  [ "$(grep -c '^[<>]' <<<"$d")" -eq 2 ] || fail "$svc: expected exactly one changed line, got:
$d"
  grep -qx "> *tag: \"$tag\"" <<<"$d" || fail "$svc: new line is not tag: \"$tag\":
$d"
  # The changed line is the one inside the image: block.
  awk '/^image:$/ {f=1; next} f && /^[^ ]/ {f=0} f && /^  tag: / {print}' "$work/$svc.yaml" |
    grep -qx "  tag: \"$tag\"" || fail "$svc: image.tag is not \"$tag\""
done

# A second run changes nothing.
mkdir "$tmp/once"
cp "$work"/*.yaml "$tmp/once/"
run "$tag" >/dev/null || fail "second bump to $tag failed"
same_as "$tmp/once" || fail "a second run with the same tag changed a file"

# Back to dev, then a full 40-character SHA: both valid.
run dev >/dev/null || fail "bump to dev failed"
run 0123456789abcdef0123456789abcdef01234567 >/dev/null || fail "bump to a 40-character SHA failed"
cp "$tmp/once"/*.yaml "$work/"

# Bad tags fail and modify nothing.
for bad in "" latest 0123ab ABCDEF1 "0123abc " 0123456789abcdef0123456789abcdef012345678 "dev;x" "a/b1234"; do
  if run "$bad" >/dev/null 2>&1; then fail "bad tag '$bad' was accepted"; fi
  same_as "$tmp/once" || fail "bad tag '$bad' modified a file"
done
if VALUES_DIR=$work "$bump" >/dev/null 2>&1; then fail "no tag was accepted"; fi
if run 0123abc extra >/dev/null 2>&1; then fail "two arguments were accepted"; fi

# A file without image.tag fails the whole run before any file is modified.
sed -i.bak '/^  tag: /d' "$work/loadgen.yaml" && rm "$work/loadgen.yaml.bak"
cp "$work/loadgen.yaml" "$tmp/once/loadgen.yaml"
msg=$(run 4567def 2>&1 >/dev/null) && fail "a file without image.tag was accepted"
grep -q 'loadgen.yaml' <<<"$msg" || fail "error does not name the bad file: $msg"
same_as "$tmp/once" || fail "a failed run (file without image.tag) modified a file"

# A tag: line outside the image: block does not count.
printf 'other:\n  tag: x\n' >>"$work/loadgen.yaml"
cp "$work/loadgen.yaml" "$tmp/once/loadgen.yaml"
if run 4567def >/dev/null 2>&1; then fail "a tag: line outside image: was accepted"; fi
same_as "$tmp/once" || fail "a failed run (tag outside image:) modified a file"

echo "BUMP OK"
