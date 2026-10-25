#!/usr/bin/env bash
# LensDock installer: installs a release of the lensdock CLI and, on a machine
# that has no LensDock deployment yet, initializes and starts it with Docker.
#
#   curl -fsSL https://github.com/JiaWeiXie/lensdock/releases/latest/download/install.sh | bash
#   gh release download --repo JiaWeiXie/lensdock --pattern install.sh --output - | bash   # private repository
#   ... | bash -s -- --install-only      # CLI only: no Docker, no state
#   ... | bash -s -- --version 1.2.3     # a specific release instead of the latest
#
# Run with --help for the options and environment variables.
set -euo pipefail

usage() {
	cat <<'EOF'
Usage: install.sh [--version X.Y.Z] [--install-only] [--help]

Downloads a LensDock release, verifies it against the release's SHA256SUMS,
installs it next to earlier versions and links the CLI. When no deployment exists
in the state directory, it then runs `lensdock admin product-init` and
`lensdock admin product-start` (Docker is required; the images are pulled and
pinned once). An existing deployment is never changed or upgraded.

Options:
  --version X.Y.Z   install this release instead of the latest (v prefix optional)
  --install-only    install the CLI only: no Docker checks, no init, no start
  -h, --help        show this help

Environment (all optional):
  LENSDOCK_VERSION          same as --version
  LENSDOCK_REPO             GitHub repository (default JiaWeiXie/lensdock)
  LENSDOCK_INSTALL_DIR      versions are installed in <dir>/<version> (default ~/.local/lib/lensdock)
  LENSDOCK_BIN_DIR          directory of the `lensdock` link (default ~/.local/bin)
  LENSDOCK_STATE_DIR        deployment state (default ~/.local/share/lensdock)
  LENSDOCK_RELEASE_BASE_URL release mirror in the GitHub layout:
                            <url>/latest/download/<asset> and <url>/download/v<version>/<asset>
  LENSDOCK_INSTALL_ARCHIVE  a local lensdock_<version>_<os>_<arch>.tar.gz; a SHA256SUMS
                            file must sit next to it; nothing is downloaded

Passed on to `lensdock admin product-init` / `product-start`:
  LENSDOCK_MODE (NOVNC|AGENT), LENSDOCK_PUBLIC_ORIGIN (http://127.0.0.1:PORT),
  LENSDOCK_COMPOSE_PROJECT, LENSDOCK_RUNTIME_IMAGE, LENSDOCK_GATEWAY_IMAGE,
  LENSDOCK_PULL (always|missing|never)

Private repository: install the GitHub CLI and run `gh auth login` (or export GH_TOKEN).
The installer uses `gh` whenever it is installed and authenticated, and plain curl otherwise.
EOF
}

log() { printf 'lensdock-install: %s\n' "$*"; }
die() {
	printf 'lensdock-install: error: %s\n' "$*" >&2
	exit 1
}

TMP=""
STAGE=""
cleanup() {
	[ -z "$STAGE" ] || rm -rf "$STAGE"
	[ -z "$TMP" ] || rm -rf "$TMP"
}

abs_path() {
	case "$1" in
	/*) printf '%s' "${1%/}" ;;
	*) printf '%s/%s' "$PWD" "${1%/}" ;;
	esac
}

need() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but was not found"; }

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

parse_args() {
	VERSION="${LENSDOCK_VERSION:-}"
	INSTALL_ONLY=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value"
			VERSION="$2"
			shift 2
			;;
		--version=*)
			VERSION="${1#--version=}"
			shift
			;;
		--install-only)
			INSTALL_ONLY=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown option: $1 (see --help)" ;;
		esac
	done
	VERSION="${VERSION#v}"
	if [ -n "$VERSION" ] && ! [[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
		die "invalid version '$VERSION' (expected X.Y.Z)"
	fi
}

resolve_environment() {
	[ -n "${HOME:-}" ] || die "HOME is not set"
	REPO="${LENSDOCK_REPO:-JiaWeiXie/lensdock}"
	[[ $REPO =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "invalid LENSDOCK_REPO '$REPO' (expected owner/name)"
	INSTALL_DIR="$(abs_path "${LENSDOCK_INSTALL_DIR:-$HOME/.local/lib/lensdock}")"
	BIN_DIR="$(abs_path "${LENSDOCK_BIN_DIR:-$HOME/.local/bin}")"
	STATE_DIR="$(abs_path "${LENSDOCK_STATE_DIR:-$HOME/.local/share/lensdock}")"
	PROJECT="${LENSDOCK_COMPOSE_PROJECT:-lensdock-product}"
	[[ $PROJECT =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || die "invalid LENSDOCK_COMPOSE_PROJECT '$PROJECT'"

	case "$(uname -s)" in
	Linux) OS=linux ;;
	Darwin) OS=darwin ;;
	*) die "unsupported operating system '$(uname -s)': Linux and macOS are supported" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	arm64 | aarch64) ARCH=arm64 ;;
	*) die "unsupported CPU architecture '$(uname -m)': amd64 and arm64 are supported" ;;
	esac

	need tar
	need mktemp
	need awk
	need chmod
	need ln
	need mv

	if [ -n "${LENSDOCK_INSTALL_ARCHIVE:-}" ]; then
		SOURCE=archive
	elif [ -n "${LENSDOCK_RELEASE_BASE_URL:-}" ]; then
		SOURCE=url
		BASE_URL="${LENSDOCK_RELEASE_BASE_URL%/}"
		CURL_PROTO='=http,https'
	elif command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
		SOURCE=gh
	else
		SOURCE=url
		BASE_URL="https://github.com/$REPO/releases"
		CURL_PROTO='=https'
	fi
	[ "$SOURCE" != url ] || need curl
}

state_exists() {
	[ -e "$STATE_DIR/deployment.json" ] || [ -L "$STATE_DIR/deployment.json" ] || [ -e "$STATE_DIR/product" ] || [ -L "$STATE_DIR/product" ]
}

# Everything the first start needs is checked before anything is downloaded.
preflight_docker() {
	command -v docker >/dev/null 2>&1 || die "Docker is required to start LensDock but 'docker' was not found. Install Docker Desktop, OrbStack or Docker Engine, or use --install-only."
	docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required ('docker compose version' failed)."
	docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable. Start Docker and run the installer again."
	local label="label=com.docker.compose.project=$PROJECT" found
	found="$(docker volume ls -q --filter "$label" 2>/dev/null || true)$(docker ps -a -q --filter "$label" 2>/dev/null || true)"
	if [ -n "$found" ]; then
		die "Docker already has containers or volumes of the Compose project '$PROJECT', but $STATE_DIR holds no deployment. Starting a fresh one would replace the credentials those services use. Set LENSDOCK_COMPOSE_PROJECT to another name (and LENSDOCK_PUBLIC_ORIGIN to a free port), or use the existing deployment's state directory."
	fi
}

fetch_failed() {
	die "could not download the release files. If the repository is private, install the GitHub CLI (https://cli.github.com) and run 'gh auth login' (or export GH_TOKEN), then run the installer again."
}

# fetch TAG ASSET DEST: TAG is empty for the latest release.
fetch() {
	local tag="$1" asset="$2" dest="$3" url
	log "downloading $asset"
	case "$SOURCE" in
	gh)
		if [ -n "$tag" ]; then
			gh release download "$tag" --repo "$REPO" --pattern "$asset" --output "$dest" </dev/null || fetch_failed
		else
			gh release download --repo "$REPO" --pattern "$asset" --output "$dest" </dev/null || fetch_failed
		fi
		;;
	url)
		if [ -n "$tag" ]; then
			url="$BASE_URL/download/$tag/$asset"
		else
			url="$BASE_URL/latest/download/$asset"
		fi
		curl --fail --silent --show-error --location --retry 3 --proto "$CURL_PROTO" --proto-redir "$CURL_PROTO" --output "$dest" "$url" </dev/null || fetch_failed
		;;
	esac
}

# obtain_release sets VERSION, ARCHIVE (a local file) and SUMS (its SHA256SUMS).
obtain_release() {
	local name suffix names
	suffix="_${OS}_${ARCH}.tar.gz"
	if [ "$SOURCE" = archive ]; then
		ARCHIVE="$LENSDOCK_INSTALL_ARCHIVE"
		[ -f "$ARCHIVE" ] || die "LENSDOCK_INSTALL_ARCHIVE '$ARCHIVE' is not a file"
		name="$(basename "$ARCHIVE")"
		[[ $name == lensdock_*"$suffix" ]] || die "'$name' is not a lensdock_<version>${suffix} archive for this machine"
		name="${name#lensdock_}"
		VERSION="${name%"$suffix"}"
		SUMS="$(dirname "$ARCHIVE")/SHA256SUMS"
		[ -f "$SUMS" ] || die "$(dirname "$ARCHIVE")/SHA256SUMS must sit next to the archive"
	else
		SUMS="$TMP/SHA256SUMS"
		if [ -z "$VERSION" ]; then
			fetch "" SHA256SUMS "$SUMS"
			names="$(awk -v suf="$suffix" '{ n = $2; sub(/^\*/, "", n); if (index(n, "lensdock_") == 1 && substr(n, length(n) - length(suf) + 1) == suf) print n }' "$SUMS")"
			[ -n "$names" ] || die "the latest release has no archive for ${OS}/${ARCH}"
			case "$names" in *$'\n'*) die "the latest release lists several archives for ${OS}/${ARCH}" ;; esac
			name="${names#lensdock_}"
			VERSION="${name%"$suffix"}"
		else
			fetch "v$VERSION" SHA256SUMS "$SUMS"
		fi
		ARCHIVE="$TMP/lensdock_${VERSION}${suffix}"
		fetch "v$VERSION" "lensdock_${VERSION}${suffix}" "$ARCHIVE"
	fi
	[[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || die "the release has an invalid version '$VERSION'"
}

verify_archive() {
	local name expected actual
	name="lensdock_${VERSION}_${OS}_${ARCH}.tar.gz"
	expected="$(awk -v name="$name" '{ n = $2; sub(/^\*/, "", n); if (n == name) print $1 }' "$SUMS")"
	[[ $expected =~ ^[0-9a-f]{64}$ ]] || die "SHA256SUMS has no single valid checksum for $name"
	actual="$(sha256_of "$ARCHIVE")"
	[ "$actual" = "$expected" ] || die "checksum mismatch for $name: expected $expected, got $actual. Nothing was installed."
	log "checksum of $name verified"

	# The archive is verified; still refuse anything that could write outside the install
	# directory or create links, devices or other special files.
	tar -tzf "$ARCHIVE" >"$TMP/list" || die "cannot read $name"
	tar -tvzf "$ARCHIVE" >"$TMP/list-verbose" || die "cannot read $name"
	if awk '$0 ~ /^\// || $0 ~ /(^|\/)\.\.(\/|$)/ { bad = 1 } END { exit bad ? 0 : 1 }' "$TMP/list"; then
		die "$name contains an unsafe path"
	fi
	if awk 'substr($0, 1, 1) !~ /[-d]/ { bad = 1 } END { exit bad ? 0 : 1 }' "$TMP/list-verbose"; then
		die "$name contains links or special files"
	fi
}

install_version() {
	local f
	mkdir -p "$INSTALL_DIR"
	STAGE="$(mktemp -d "$INSTALL_DIR/.stage.XXXXXX")"
	tar -xzf "$ARCHIVE" -C "$STAGE" --no-same-owner
	chmod -R u+rwX,go+rX,go-w "$STAGE"
	for f in lensdock VERSION docker/compose.yaml docker/browser/seccomp/chromium.json bin/linux-amd64/lensdock bin/linux-arm64/lensdock; do
		if [ ! -f "$STAGE/$f" ] || [ -L "$STAGE/$f" ]; then
			die "the archive is missing $f"
		fi
	done
	[ "$(tr -d '[:space:]' <"$STAGE/VERSION")" = "$VERSION" ] || die "the archive's VERSION file does not say $VERSION"
	chmod 755 "$STAGE" "$STAGE/lensdock" "$STAGE/bin/linux-amd64/lensdock" "$STAGE/bin/linux-arm64/lensdock"

	VDIR="$INSTALL_DIR/$VERSION"
	if [ -e "$VDIR" ] || [ -L "$VDIR" ]; then
		# An earlier install of the same version is kept as it is.
		if [ ! -f "$VDIR/VERSION" ] || [ ! -x "$VDIR/lensdock" ] || [ "$(tr -d '[:space:]' <"$VDIR/VERSION")" != "$VERSION" ]; then
			die "$VDIR exists but is not a complete LensDock $VERSION install; move it away and run the installer again"
		fi
		log "version $VERSION is already installed in $VDIR; keeping it"
		rm -rf "$STAGE"
	else
		mv "$STAGE" "$VDIR"
		log "installed $VERSION in $VDIR"
	fi
	STAGE=""
	"$VDIR/lensdock" version >/dev/null </dev/null || die "$VDIR/lensdock does not run on this machine"
}

link_cli() {
	local link="$BIN_DIR/lensdock" target tmp
	mkdir -p "$BIN_DIR"
	if [ -L "$link" ]; then
		target="$(readlink "$link")"
		case "$target" in
		"$INSTALL_DIR"/*) ;;
		*) die "$link is a link to $target, which this installer does not manage; remove it or set LENSDOCK_BIN_DIR" ;;
		esac
	elif [ -e "$link" ]; then
		die "$link exists and is not a link managed by this installer; remove it or set LENSDOCK_BIN_DIR"
	fi
	tmp="$BIN_DIR/.lensdock.$$"
	ln -s "$VDIR/lensdock" "$tmp"
	mv -f "$tmp" "$link"
	log "linked $link -> $VDIR/lensdock"
	case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*) log "note: $BIN_DIR is not on your PATH; add it with: export PATH=\"$BIN_DIR:\$PATH\"" ;;
	esac
}

start_product() {
	local cli="$VDIR/lensdock"
	log "initializing $STATE_DIR"
	"$cli" admin product-init --state-dir "$STATE_DIR" </dev/null ||
		die "initialization failed and nothing was started. Fix the problem above and run the installer again."
	log "pulling and pinning the images, then starting LensDock (the first start can take a few minutes)"
	"$cli" admin product-start --state-dir "$STATE_DIR" </dev/null ||
		die "the first start failed. The CLI is installed and $STATE_DIR is initialized; after fixing the problem above run: $BIN_DIR/lensdock admin product-start --state-dir \"$STATE_DIR\""
}

main() {
	parse_args "$@"
	resolve_environment

	START=0
	if [ "$INSTALL_ONLY" -eq 0 ]; then
		if state_exists; then
			log "a deployment already exists in $STATE_DIR: it will not be changed or started"
		else
			preflight_docker
			START=1
		fi
	fi

	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	TMP="$(mktemp -d "${TMPDIR:-/tmp}/lensdock-install.XXXXXX")"

	obtain_release
	verify_archive
	install_version
	link_cli

	if [ "$START" -eq 1 ]; then
		start_product
	fi

	log "done: LensDock $VERSION, CLI $BIN_DIR/lensdock, state $STATE_DIR"
	if [ "$INSTALL_ONLY" -eq 1 ]; then
		log "next: $BIN_DIR/lensdock admin product-init --state-dir \"$STATE_DIR\" && $BIN_DIR/lensdock admin product-start --state-dir \"$STATE_DIR\""
	elif [ "$START" -eq 0 ]; then
		log "the existing deployment keeps its images and data. Start it with: $BIN_DIR/lensdock admin product-start --state-dir \"$STATE_DIR\"; upgrade it with 'lensdock update' (see the README)"
	else
		log "stop: $BIN_DIR/lensdock update compose --deployment \"$STATE_DIR/deployment.json\" -- down   start again: $BIN_DIR/lensdock admin product-start --state-dir \"$STATE_DIR\""
	fi
}

main "$@" </dev/null
