#!/usr/bin/env bash
# Print the requested semantic-version increment from a published release tag.
set -euo pipefail

usage() {
  printf 'usage: %s <MAJOR.MINOR.PATCH|vMAJOR.MINOR.PATCH> <patch|minor|major>\n' "${0##*/}" >&2
  exit 2
}

[ "$#" = 2 ] || usage
LATEST_TAG="$1"
BUMP="$2"
VERSION="${LATEST_TAG#v}"

if ! [[ "$VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  printf 'error: unsupported semantic version: %s\n' "$LATEST_TAG" >&2
  exit 1
fi

increment_decimal() {
  local value="$1" index digit next carry=1 result=""
  for ((index=${#value}-1; index>=0; index--)); do
    digit="${value:index:1}"
    if [ "$carry" = 1 ]; then
      case "$digit" in
        0) next=1; carry=0 ;;
        1) next=2; carry=0 ;;
        2) next=3; carry=0 ;;
        3) next=4; carry=0 ;;
        4) next=5; carry=0 ;;
        5) next=6; carry=0 ;;
        6) next=7; carry=0 ;;
        7) next=8; carry=0 ;;
        8) next=9; carry=0 ;;
        9) next=0 ;;
      esac
    else
      next="$digit"
    fi
    result="$next$result"
  done
  [ "$carry" = 0 ] || result="1$result"
  printf '%s\n' "$result"
}

IFS=. read -r MAJOR MINOR PATCH <<< "$VERSION"
case "$BUMP" in
  patch) PATCH="$(increment_decimal "$PATCH")" ;;
  minor) MINOR="$(increment_decimal "$MINOR")"; PATCH=0 ;;
  major) MAJOR="$(increment_decimal "$MAJOR")"; MINOR=0; PATCH=0 ;;
  *)
    printf 'error: unsupported version bump: %s\n' "$BUMP" >&2
    exit 1
    ;;
esac

printf '%s.%s.%s\n' "$MAJOR" "$MINOR" "$PATCH"
