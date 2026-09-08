#!/bin/sh
#
# GitOne installer - https://gitone.io
#
#   curl -fsSL https://gitone.io/install.sh | sh
#
# It downloads the binary for your platform from the latest GitHub release,
# verifies its SHA-256 against the checksums.txt published with that release,
# and installs nothing if the checksum does not match. It never calls sudo.
#
# Environment:
#   GITONE_VERSION       install this tag instead of the latest, e.g. v0.1.0
#   GITONE_INSTALL_DIR   install here instead of $HOME/.local/bin
#
set -eu

REPO_URL="https://github.com/devidevio/gitone"
INSTALL_DIR="${GITONE_INSTALL_DIR:-$HOME/.local/bin}"

BOLD=''
DIM=''
RED=''
GREEN=''
RESET=''
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	BOLD=$(printf '\033[1m')
	DIM=$(printf '\033[90m')
	RED=$(printf '\033[31m')
	GREEN=$(printf '\033[32m')
	RESET=$(printf '\033[0m')
fi

say() { printf '%s\n' "$*"; }
note() { printf '%s%s%s\n' "$DIM" "$*" "$RESET"; }
ok() { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$*"; }

die() {
	printf '%s✗%s %s\n' "$RED" "$RESET" "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but was not found on PATH."
}

# ---------------------------------------------------------------- platform --

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)

	case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "Unsupported operating system: $os. GitOne supports Linux and macOS." ;;
	esac

	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "Unsupported architecture: $arch. GitOne supports amd64 and arm64." ;;
	esac

	printf '%s_%s' "$os" "$arch"
}

# Resolves the tag by following the /releases/latest redirect, so the script
# does not need the GitHub API and cannot be rate limited.
resolve_version() {
	if [ -n "${GITONE_VERSION:-}" ]; then
		printf '%s' "$GITONE_VERSION"
		return
	fi
	resolved=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REPO_URL/releases/latest" 2>/dev/null) ||
		die "Could not reach GitHub to determine the latest release."
	version="${resolved##*/}"
	case "$version" in
	v*) printf '%s' "$version" ;;
	*) die "Could not determine the latest release. Set GITONE_VERSION to install a specific tag." ;;
	esac
}

# ------------------------------------------------------------ verification --

# One checksum tool, whichever this machine has. Both read the same file.
verify_checksum() {
	file="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum --check --ignore-missing checksums.txt >/dev/null 2>&1
		return $?
	fi
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 -c --ignore-missing checksums.txt >/dev/null 2>&1
		return $?
	fi
	die "Neither sha256sum nor shasum is available, so the download cannot be verified. Install one, or download and verify the release manually: $REPO_URL/releases"
}

# ------------------------------------------------------------------- main --

need curl
need uname
need mkdir
need install

platform=$(detect_platform)
version=$(resolve_version)
asset="gitone_${version}_${platform}"
base="$REPO_URL/releases/download/$version"

say ""
say "${BOLD}GitOne $version${RESET}  ${DIM}$platform${RESET}"
say ""

workdir=$(mktemp -d 2>/dev/null || mktemp -d -t gitone)
trap 'rm -rf "$workdir"' EXIT INT TERM
cd "$workdir"

note "Downloading $asset"
curl -fsSL -o "$asset" "$base/$asset" ||
	die "Download failed: $base/$asset"

note "Downloading checksums.txt"
curl -fsSL -o checksums.txt "$base/checksums.txt" ||
	die "This release publishes no checksums.txt, so the download cannot be verified. Nothing was installed."

if verify_checksum "$asset"; then
	ok "SHA-256 verified"
else
	die "Checksum mismatch for $asset. Nothing was installed. Report this at $REPO_URL/issues"
fi

mkdir -p "$INSTALL_DIR" || die "Could not create $INSTALL_DIR"
install -m 0755 "$asset" "$INSTALL_DIR/gitone" ||
	die "Could not write to $INSTALL_DIR. Set GITONE_INSTALL_DIR to a directory you own."
ok "Installed $INSTALL_DIR/gitone"

say ""
if command -v gitone >/dev/null 2>&1 && [ "$(command -v gitone)" = "$INSTALL_DIR/gitone" ]; then
	"$INSTALL_DIR/gitone" --version
	say ""
	say "Next: ${BOLD}cd${RESET} into a project and run ${BOLD}gitone setup${RESET}"
else
	if command -v gitone >/dev/null 2>&1; then
		say "Your PATH selects $(command -v gitone) instead of $INSTALL_DIR/gitone."
	else
		say "$INSTALL_DIR is not on your PATH."
	fi
	say "Put the new installation first:"
	say ""
	say "  export PATH=\"$INSTALL_DIR:\$PATH\""
	say ""
	note "Append that line to ~/.zshrc or ~/.bashrc to make it permanent."
	say "Then verify:"
	say "  gitone --version"
	say "Next: cd into a project and run gitone setup"
fi

say ""
note "Optional shorter commands:"
note "  ln -s \"$INSTALL_DIR/gitone\" \"$INSTALL_DIR/git1\""
note "  ln -s \"$INSTALL_DIR/gitone\" \"$INSTALL_DIR/g1\""
say ""
