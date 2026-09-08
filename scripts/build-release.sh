#!/bin/sh
# Builds the release matrix into dist/, writes SHA-256 checksums and verifies
# that the host-native binary reports the version it was stamped with.
#
# Usage: scripts/build-release.sh <version> [vsix]
#
# The GitHub tag workflow runs exactly this script, so a local run uses the same
# build matrix and artifact names.
set -eu

version="${1:-devel}"
vsix="${2:-}"
module=github.com/devidevio/gitone
output=dist

rm -rf "$output"
mkdir -p "$output"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	goos="${target%/*}"
	goarch="${target#*/}"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
		-trimpath \
		-buildvcs=false \
		-ldflags "-s -w -X ${module}/internal/cli.version=${version}" \
		-o "${output}/gitone_${version}_${goos}_${goarch}" \
		./cmd/gitone
done

if [ -n "$vsix" ]; then
	if [ ! -f "$vsix" ]; then
		echo "release: VSIX does not exist: $vsix" >&2
		exit 1
	fi
	cp "$vsix" "${output}/gitone_${version}_vscode.vsix"
fi

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$output" && sha256sum gitone_* >checksums.txt)
else
	(cd "$output" && shasum -a 256 gitone_* >checksums.txt)
fi

# The -X path is a string the compiler does not check: a renamed package would
# silently leave the version empty, so the native binary has to say the tag.
native="${output}/gitone_${version}_$(go env GOOS)_$(go env GOARCH)"
if [ -x "$native" ]; then
	reported="$("$native" --version)"
	if [ "$reported" != "gitone ${version}" ]; then
		echo "release: ${native} reports \"${reported}\", want \"gitone ${version}\"" >&2
		exit 1
	fi
fi

cat "${output}/checksums.txt"
