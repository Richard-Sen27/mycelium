#!/bin/sh
# Install mycel for the current user on macOS or Linux. Never needs sudo.
#
#   curl -fsSL https://raw.githubusercontent.com/Richard-Sen27/mycelium/main/scripts/install.sh | sh
#   sh install.sh [--version v1.2.3] [--from-source [--ref <git ref>]] [--dry-run]
#
# It downloads mycel_<version>_<os>_<arch>.tar.gz and checksums.txt from the
# GitHub release over HTTPS only, verifies the SHA-256 checksum and installs
# the binary to $MYCEL_INSTALL_DIR (default ~/.local/bin). --from-source
# clones the repository and builds it with Go 1.27 instead (the web UI is
# embedded when pnpm is available). --dry-run prints every step without
# doing it.
#
# Environment: MYCEL_REPO (default Richard-Sen27/mycelium),
# MYCEL_INSTALL_DIR, GITHUB_TOKEN (optional, raises the API rate limit; never
# put on a command line), MYCEL_DOWNLOAD_BASE (a mirror of the
# releases/download tree; downloads from it may use any protocol, the tests
# use a file:// URL).
#
# The whole script is a function called on the last line, so a download cut
# short by `curl | sh` runs nothing.

set -eu

REPO="${MYCEL_REPO:-Richard-Sen27/mycelium}"
MODULE="github.com/richard-sen27/mycelium"
INSTALL_DIR="${MYCEL_INSTALL_DIR:-${HOME}/.local/bin}"
VERSION=""
FROM_SOURCE=0
REF=""
DRY_RUN=0
GO_MIN_MINOR=27
TMP=""
OS=""
ARCH=""

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: %s\n' "$*" >&2; }
die() { err "$*"; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [--version vX.Y.Z] [--from-source [--ref <git ref>]] [--dry-run]

  --version v1.2.3   install this release instead of the latest
  --from-source      build from the repository with Go 1.27 (git required)
  --ref <ref>        branch or tag to build with --from-source (default: the
                     --version tag, else main)
  --dry-run          print what would be done, change nothing
  --repo owner/name  GitHub repository (default Richard-Sen27/mycelium)
  --install-dir dir  where to put mycel (default ~/.local/bin or $MYCEL_INSTALL_DIR)
EOF
}

# run prints a command and runs it unless --dry-run.
run() {
	say "+ $*"
	if [ "$DRY_RUN" -eq 0 ]; then
		"$@"
	fi
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

cleanup() {
	if [ -n "$TMP" ] && [ -d "$TMP" ]; then
		rm -rf "$TMP"
	fi
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value"
			VERSION="$2"
			shift 2
			;;
		--version=*) VERSION="${1#*=}"; shift ;;
		--from-source) FROM_SOURCE=1; shift ;;
		--ref)
			[ $# -ge 2 ] || die "--ref needs a value"
			REF="$2"
			shift 2
			;;
		--ref=*) REF="${1#*=}"; shift ;;
		--dry-run) DRY_RUN=1; shift ;;
		--repo)
			[ $# -ge 2 ] || die "--repo needs a value"
			REPO="$2"
			shift 2
			;;
		--install-dir)
			[ $# -ge 2 ] || die "--install-dir needs a value"
			INSTALL_DIR="$2"
			shift 2
			;;
		-h | --help) usage; exit 0 ;;
		*) usage >&2; die "unknown argument: $1" ;;
		esac
	done
	if [ -n "$VERSION" ]; then
		case "$VERSION" in
		v*) ;;
		*) VERSION="v$VERSION" ;;
		esac
	fi
	case "$INSTALL_DIR" in
	/*) ;;
	"") die "the install directory is empty" ;;
	*) INSTALL_DIR="$(pwd)/$INSTALL_DIR" ;;
	esac
}

detect_platform() {
	case "$(uname -s)" in
	Darwin) OS=darwin ;;
	Linux) OS=linux ;;
	*) die "unsupported OS $(uname -s): use install.ps1 on Windows, or --from-source" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	arm64 | aarch64) ARCH=arm64 ;;
	*) die "unsupported CPU $(uname -m) (release builds: amd64, arm64)" ;;
	esac
	# A shell running under Rosetta reports x86_64 on Apple silicon: install
	# the native build.
	if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		ARCH=arm64
	fi
}

# fetch URL FILE downloads with curl or wget: HTTPS only (TLS 1.2+) unless
# the URL is under MYCEL_DOWNLOAD_BASE. The GitHub token goes to curl on
# stdin and to wget in a 0600 config file, never on a command line.
fetch() {
	say "+ download $1 -> $2"
	[ "$DRY_RUN" -eq 0 ] || return 0
	secure=1
	if [ -n "${MYCEL_DOWNLOAD_BASE:-}" ] && [ "${1#"$MYCEL_DOWNLOAD_BASE"}" != "$1" ]; then
		secure=0
	fi
	auth=0
	if [ -n "${GITHUB_TOKEN:-}" ] && [ "${1#https://api.github.com/}" != "$1" ]; then
		auth=1
	fi
	url="$1"
	out="$2"
	if command -v curl >/dev/null 2>&1; then
		set -- -fsSL --retry 3
		if [ "$secure" -eq 1 ]; then
			set -- "$@" --proto '=https' --proto-redir '=https' --tlsv1.2
		fi
		if [ "$auth" -eq 1 ]; then
			printf 'header = "Authorization: Bearer %s"\n' "$GITHUB_TOKEN" |
				curl "$@" -K - -o "$out" "$url" || die "download failed: $url"
		else
			curl "$@" -o "$out" "$url" || die "download failed: $url"
		fi
	elif command -v wget >/dev/null 2>&1; then
		set -- -q
		if [ "$secure" -eq 1 ]; then
			set -- "$@" --https-only --secure-protocol=TLSv1_2
		fi
		if [ "$auth" -eq 1 ]; then
			(umask 077 && printf 'header = Authorization: Bearer %s\n' "$GITHUB_TOKEN" >"$TMP/wgetrc")
			set -- "$@" --config="$TMP/wgetrc"
		fi
		wget "$@" -O "$out" "$url" || die "download failed: $url"
	else
		die "curl or wget is required"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

install_release() {
	if [ -z "$VERSION" ]; then
		api="https://api.github.com/repos/${REPO}/releases/latest"
		fetch "$api" "$TMP/latest.json"
		if [ "$DRY_RUN" -eq 1 ]; then
			VERSION="<latest>"
		else
			VERSION="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/latest.json" | head -n 1)"
			[ -n "$VERSION" ] || die "no release found at $api (try --version or --from-source)"
		fi
	fi
	num="${VERSION#v}"
	archive="mycel_${num}_${OS}_${ARCH}.tar.gz"
	base="${MYCEL_DOWNLOAD_BASE:-https://github.com/${REPO}/releases/download}/${VERSION}"
	say "release: ${VERSION} (${archive})"
	fetch "$base/$archive" "$TMP/$archive"
	fetch "$base/checksums.txt" "$TMP/checksums.txt"

	say "+ verify sha256 of $archive against checksums.txt"
	if [ "$DRY_RUN" -eq 0 ]; then
		want="$(awk -v f="$archive" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt" | head -n 1)"
		[ -n "$want" ] || die "$archive is not listed in checksums.txt"
		got="$(sha256 "$TMP/$archive")"
		[ "$want" = "$got" ] || die "checksum mismatch for $archive: expected $want, got $got"
		say "checksum ok: $got"
	fi
	run tar -xzf "$TMP/$archive" -C "$TMP"
	run mkdir -p "$INSTALL_DIR"
	run cp "$TMP/mycel" "$INSTALL_DIR/mycel.new"
	run chmod 0755 "$INSTALL_DIR/mycel.new"
	run mv -f "$INSTALL_DIR/mycel.new" "$INSTALL_DIR/mycel"
}

install_source() {
	need git
	if [ "$DRY_RUN" -eq 1 ]; then
		say "+ check: go version is 1.${GO_MIN_MINOR} or newer"
	else
		need go
		gov="$(go env GOVERSION 2>/dev/null || true)"
		minor="$(printf '%s' "$gov" | sed -n 's/^go1\.\([0-9][0-9]*\).*/\1/p')"
		[ -n "$minor" ] && [ "$minor" -ge "$GO_MIN_MINOR" ] ||
			die "Go 1.${GO_MIN_MINOR} or newer is required for --from-source (found ${gov:-none}); see https://go.dev/dl/"
	fi
	ref="${REF:-${VERSION:-main}}"
	src="$TMP/src"
	run git clone --quiet --depth 1 --branch "$ref" "https://github.com/${REPO}.git" "$src"
	if command -v pnpm >/dev/null 2>&1; then
		say "+ (cd $src/web && pnpm install --frozen-lockfile && pnpm build)"
		if [ "$DRY_RUN" -eq 0 ]; then
			(cd "$src/web" && pnpm install --frozen-lockfile && pnpm build)
		fi
	else
		say "pnpm not found: building without the web UI (install Node 22 and pnpm to embed it)"
	fi
	run mkdir -p "$INSTALL_DIR"
	say "+ (cd $src && go build -ldflags \"-X ${MODULE}/internal/version.Version=${ref}\" -o $INSTALL_DIR/mycel ./cmd/mycel)"
	if [ "$DRY_RUN" -eq 0 ]; then
		(cd "$src" && CGO_ENABLED=0 go build -ldflags "-s -w -X ${MODULE}/internal/version.Version=${ref}" -o "$INSTALL_DIR/mycel.new" ./cmd/mycel)
		mv -f "$INSTALL_DIR/mycel.new" "$INSTALL_DIR/mycel"
	fi
}

main() {
	parse_args "$@"
	detect_platform

	[ "$DRY_RUN" -eq 1 ] && say "dry run: nothing is downloaded, built or installed"
	say "platform: ${OS}/${ARCH}; install dir: ${INSTALL_DIR}"

	trap cleanup EXIT
	trap 'cleanup; exit 130' INT TERM
	if [ "$DRY_RUN" -eq 0 ]; then
		TMP="$(mktemp -d 2>/dev/null || mktemp -d -t mycel)"
	else
		TMP="<tmp>"
	fi

	if [ "$FROM_SOURCE" -eq 1 ]; then
		install_source
	else
		install_release
	fi

	if [ "$DRY_RUN" -eq 0 ]; then
		"$INSTALL_DIR/mycel" version >/dev/null || die "the installed binary does not run: $INSTALL_DIR/mycel"
		say "installed: $INSTALL_DIR/mycel ($("$INSTALL_DIR/mycel" version))"
	else
		say "would install: $INSTALL_DIR/mycel"
	fi

	case ":${PATH}:" in
	*":${INSTALL_DIR}:"*) ;;
	*)
		say ""
		say "$INSTALL_DIR is not on your PATH. Add it, e.g.:"
		say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.profile   # or ~/.zshrc"
		;;
	esac

	say ""
	say "Next:"
	say "  joining a network:   mycel join <token>   then   mycel service install"
	say "  starting a network:  mycel hub init --advertise https://<this-host>:7777   then   mycel service install"
	say "  quickstart:          https://github.com/${REPO}/blob/main/docs/quickstart.md"
}

main "$@"
