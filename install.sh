#!/bin/sh
# Installer for the cloudconsole CLI.
#
#   curl -fsSL https://raw.githubusercontent.com/studio-ch/cloudconsole-cli/main/install.sh | sh
#
# POSIX sh on purpose: this runs inside minimal CI images that have no
# bash. Verifies the download against SHA256SUMS and refuses to continue
# if it cannot — a silently unverified binary is worse than a failed
# install. Never invokes sudo on its own.
#
# Environment:
#   CLOUDCONSOLE_VERSION      install a specific tag, e.g. v0.1.0 (default: latest)
#   CLOUDCONSOLE_INSTALL_DIR  where to put the binary (default: /usr/local/bin,
#                       falling back to $HOME/.local/bin if not writable)

set -eu

REPO="studio-ch/cloudconsole-cli"
BINARY="cloudconsole"

log()  { printf '%s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed."
}

need uname
need tar
need mktemp

# curl or wget, whichever is present.
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
    # Follows redirects and prints where it ended up, without downloading
    # the body — how the latest tag is discovered below.
    resolve_redirect() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO "$2" "$1"; }
    resolve_redirect() {
        wget -qS --spider "$1" 2>&1 \
            | sed -n 's/^[[:space:]]*Location:[[:space:]]*\([^[:space:]]*\).*/\1/p' \
            | tail -n 1
    }
else
    die "either curl or wget is required."
fi

# --- platform -------------------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$os" in
    linux)  ;;
    darwin) ;;
    *) die "unsupported operating system: $os. Download a release manually from https://github.com/$REPO/releases" ;;
esac

case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
esac

# --- version --------------------------------------------------------------

# Releases in this repository carry a plain `vX.Y.Z` tag (the monorepo's
# `cli/` prefix is stripped when publishing), while the archives are named
# with the bare "0.1.0".
#
# The latest tag comes from the /releases/latest redirect rather than the
# GitHub API: the API is aggressively rate-limited for unauthenticated
# callers, which is exactly what a shared CI runner is, and the redirect
# needs no token and no file to keep in sync.
if [ -n "${CLOUDCONSOLE_VERSION:-}" ]; then
    version="$CLOUDCONSOLE_VERSION"
    case "$version" in v*) ;; *) version="v$version" ;; esac
    tag="$version"
else
    final=$(resolve_redirect "https://github.com/$REPO/releases/latest") \
        || die "could not reach GitHub to determine the latest version."
    # .../releases/tag/v0.1.0  ->  v0.1.0
    tag=${final##*/releases/tag/}
    case "$tag" in
        v*) ;;
        *) die "could not determine the latest version (got '$final'). Set CLOUDCONSOLE_VERSION explicitly." ;;
    esac
    version="$tag"
fi

bare="${version#v}"
archive="${BINARY}_${bare}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

# --- download and verify --------------------------------------------------

tmp=$(mktemp -d)
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT INT TERM

log "Downloading $archive ($version)…"
fetch "$base/$archive" "$tmp/$archive" \
    || die "download failed. Check that $tag exists at https://github.com/$REPO/releases"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" \
    || die "could not download SHA256SUMS; refusing to install unverified."

log "Verifying checksum…"
if command -v sha256sum >/dev/null 2>&1; then
    checker="sha256sum -c -"
elif command -v shasum >/dev/null 2>&1; then
    checker="shasum -a 256 -c -"
else
    die "neither sha256sum nor shasum is available; refusing to install unverified."
fi

# Check only our archive: SHA256SUMS covers every platform, and the rest
# are not present here.
grep " ${archive}\$" "$tmp/SHA256SUMS" > "$tmp/want" \
    || die "$archive is not listed in SHA256SUMS."
( cd "$tmp" && eval "$checker" < want >/dev/null ) \
    || die "checksum mismatch for $archive. Do not use this download."

log "Checksum ok."
tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/$BINARY" ] || die "the archive did not contain a '$BINARY' binary."
chmod +x "$tmp/$BINARY"

# --- install --------------------------------------------------------------

dir="${CLOUDCONSOLE_INSTALL_DIR:-/usr/local/bin}"
if [ ! -d "$dir" ] || [ ! -w "$dir" ]; then
    fallback="$HOME/.local/bin"
    if [ -z "${CLOUDCONSOLE_INSTALL_DIR:-}" ]; then
        log "$dir is not writable; installing to $fallback instead."
        mkdir -p "$fallback"
        dir="$fallback"
    else
        die "$dir is not writable. Create it, or set CLOUDCONSOLE_INSTALL_DIR to somewhere you can write."
    fi
fi

mv "$tmp/$BINARY" "$dir/$BINARY"
log "Installed $BINARY to $dir/$BINARY"

# Tell the user if they cannot actually run it yet.
case ":$PATH:" in
    *":$dir:"*) ;;
    *) log ""
       log "  $dir is not on your PATH. Add it:"
       log "    export PATH=\"\$PATH:$dir\""
       ;;
esac

log ""
"$dir/$BINARY" version --client || true
log ""
log "Next: authenticate with an API key from the panel (Settings → API keys)"
log "  $BINARY auth login"
