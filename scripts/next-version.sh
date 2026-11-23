#!/bin/sh
# Prints the next semantic version tag (vMAJOR.MINOR.PATCH) for a bump level.
#   scripts/next-version.sh patch|minor|major [current-tag]
# With no current tag given, the highest v<N>.<N>.<N> tag in the repo is used;
# with no tags at all the base is v0.0.0, so the first release is v0.0.1,
# v0.1.0 or v1.0.0.
set -eu

bump=${1:-}
current=${2:-$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)}
current=${current:-v0.0.0}

case $current in
v[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "not a vMAJOR.MINOR.PATCH tag: $current" >&2; exit 2 ;;
esac

v=${current#v}
major=${v%%.*}; rest=${v#*.}
minor=${rest%%.*}; patch=${rest#*.}

case $bump in
major) major=$((major + 1)); minor=0; patch=0 ;;
minor) minor=$((minor + 1)); patch=0 ;;
patch) patch=$((patch + 1)) ;;
*) echo "usage: $0 patch|minor|major [current-tag]" >&2; exit 2 ;;
esac

echo "v$major.$minor.$patch"
