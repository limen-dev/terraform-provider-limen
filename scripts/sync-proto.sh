#!/usr/bin/env bash
# Copies limen.manage.v1 from a Limen checkout and checks that the Auth
# subset in proto/limen/v1/auth.proto still matches Limen's messages.
#
#   scripts/sync-proto.sh [path to limen checkout] [git ref]   (defaults: ../limen origin/main)
#   scripts/sync-proto.sh --check ...                          (fail instead of copying)
set -euo pipefail

check=false
if [[ "${1:-}" == "--check" ]]; then check=true; shift; fi
limen="${1:-../limen}"
ref="${2:-origin/main}"
here="$(cd "$(dirname "$0")/.." && pwd)"

upstream="$(git -C "$limen" show "$ref:proto/limen/manage/v1/manage.proto")"
if $check; then
  if ! diff -u <(printf '%s\n' "$upstream") "$here/proto/limen/manage/v1/manage.proto"; then
    echo "manage.proto differs from $limen at $ref: run scripts/sync-proto.sh" >&2
    exit 1
  fi
else
  printf '%s\n' "$upstream" > "$here/proto/limen/manage/v1/manage.proto"
  (cd "$here" && buf generate)
  echo "copied manage.proto from $limen at $(git -C "$limen" rev-parse --short "$ref") and regenerated"
fi

# Every message in auth.proto must exist in Limen's control.proto with the
# same fields: compare "type name = number" lines.
control="$(git -C "$limen" show "$ref:proto/limen/v1/control.proto")"
fields() { awk -v m="$1" '$1=="message" && $2==m {on=1; next} on && /^}/ {exit} on && /=/ {sub(/;.*/, ""); gsub(/^ +| +$/, ""); print}'; }
status=0
while read -r m; do
  if ! diff <(fields "$m" <<<"$control") <(fields "$m" < "$here/proto/limen/v1/auth.proto") >/dev/null; then
    echo "limen.v1.$m differs from Limen's control.proto:" >&2
    diff <(fields "$m" <<<"$control") <(fields "$m" < "$here/proto/limen/v1/auth.proto") >&2 || true
    status=1
  fi
done < <(awk '$1=="message" {print $2}' "$here/proto/limen/v1/auth.proto")
exit $status
