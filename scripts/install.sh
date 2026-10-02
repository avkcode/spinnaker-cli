#!/usr/bin/env bash
#
# Install the sc binary from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/avkcode/spinnaker-cli/main/scripts/install.sh | bash
#   SC_VERSION=v0.1.0 PREFIX=~/.local bash install.sh
set -euo pipefail

REPO="avkcode/spinnaker-cli"
PREFIX="${PREFIX:-/usr/local}"
BINDIR="${PREFIX}/bin"
VERSION="${SC_VERSION:-latest}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "${arch}" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: ${arch}" ;;
esac
case "${os}" in
  linux|darwin) ;;
  *) die "unsupported OS: ${os}" ;;
esac

if [[ "${VERSION}" == "latest" ]]; then
  log "Resolving the latest release"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
  [[ -n "${VERSION}" ]] || die "could not resolve the latest release; set SC_VERSION"
fi

asset="sc_${VERSION#v}_${os}_${arch}.tar.gz"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

log "Downloading ${asset}"
curl -fsSL -o "${tmp}/${asset}" "${url}" || die "download failed: ${url}"

# Verify against the release checksums when they are published.
if curl -fsSL -o "${tmp}/checksums.txt" \
     "https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt" 2>/dev/null; then
  log "Verifying the checksum"
  if command -v sha256sum >/dev/null; then
    (cd "${tmp}" && grep " ${asset}\$" checksums.txt | sha256sum -c -) || die "checksum mismatch"
  elif command -v shasum >/dev/null; then
    (cd "${tmp}" && grep " ${asset}\$" checksums.txt | shasum -a 256 -c -) || die "checksum mismatch"
  else
    log "No sha256sum or shasum available; skipping verification"
  fi
else
  log "No checksums.txt in this release; skipping verification"
fi

tar -xzf "${tmp}/${asset}" -C "${tmp}"
[[ -f "${tmp}/sc" ]] || die "the archive did not contain an 'sc' binary"

log "Installing to ${BINDIR}/sc"
if [[ -w "${BINDIR}" ]]; then
  install -m 0755 "${tmp}/sc" "${BINDIR}/sc"
else
  command -v sudo >/dev/null || die "${BINDIR} is not writable and sudo is unavailable; set PREFIX"
  sudo install -m 0755 "${tmp}/sc" "${BINDIR}/sc"
fi

log "Installed $("${BINDIR}/sc" --version)"
cat <<EOF

Next:
  sc login --gate http://spinnaker.example.com/api/v1 --user admin --password secret
  sc doctor

Shell completion:
  source <(sc completion bash)    # or zsh / fish / powershell
EOF
