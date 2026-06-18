#!/usr/bin/env bash
#
# Cut a new release: bump the version, tag it, and publish a GitHub release.
#
#   ./release.sh build "Fix output dir bug"   # v0.9.4 -> v0.9.5   (patch)
#   ./release.sh minor "Add version flag"      # v0.9.4 -> v0.10.0
#   ./release.sh major "Stable API"            # v0.9.4 -> v1.0.0
#
# The next version is derived from the latest vMAJOR.MINOR.PATCH git tag
# (TEST-prefixed fixture tags are ignored) and the release is titled with the
# <title> argument. No binaries are attached; users install with
# `go install github.com/mevdschee/github-export@<tag>`, which bakes the version
# into the build so `github-export -version` reports it.
#
# Requires: a clean working tree on the default branch, Go, and the gh CLI
# logged in (gh auth login).

set -euo pipefail

cd "$(dirname "$0")"

level="${1:-}"
case "$level" in
	major | minor | build) ;;
	*)
		echo "usage: $0 {major|minor|build} <title>" >&2
		exit 1
		;;
esac

shift
title="$*"
if [ -z "$title" ]; then
	echo "usage: $0 {major|minor|build} <title>" >&2
	exit 1
fi

# Refuse to release a dirty tree: the tag must point at committed code.
if [ -n "$(git status --porcelain)" ]; then
	echo "error: working tree is not clean; commit or stash first" >&2
	exit 1
fi

# Must be on the default branch.
branch="$(git rev-parse --abbrev-ref HEAD)"
default="$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's#^origin/##')"
default="${default:-main}"
if [ "$branch" != "$default" ]; then
	echo "error: on branch '$branch', expected default branch '$default'" >&2
	exit 1
fi

# Make sure the build compiles and the tests pass before tagging anything.
go build ./...
go test -short ./...

# Sync tags and the default branch so the new tag sits on top of the remote.
git fetch --quiet --tags origin
if [ -n "$(git rev-list "HEAD..origin/$default" 2>/dev/null)" ]; then
	echo "error: local '$default' is behind origin/$default; pull first" >&2
	exit 1
fi

# Latest vMAJOR.MINOR.PATCH tag, ignoring TEST-prefixed fixtures.
latest="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -1)"
latest="${latest:-v0.0.0}"
IFS=. read -r major minor patch <<EOF
${latest#v}
EOF

case "$level" in
	major)
		major=$((major + 1))
		minor=0
		patch=0
		;;
	minor)
		minor=$((minor + 1))
		patch=0
		;;
	build)
		patch=$((patch + 1))
		;;
esac
next="v${major}.${minor}.${patch}"

echo "Releasing ${latest} -> ${next}: ${title}"

git tag -a "$next" -m "$title"
git push origin "$next"

gh release create "$next" --title "$title"

echo "Released ${next}: $(gh release view "$next" --json url --jq .url)"
