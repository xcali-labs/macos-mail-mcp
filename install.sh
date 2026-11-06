#!/bin/sh
set -eu

REPO="xcali-labs/macos-mail-mcp"
BIN_DIR="${HOME}/.local/bin"
BIN_PATH="${BIN_DIR}/apple-mail-mcp"

ARCH="$(uname -m)"
case "${ARCH}" in
  arm64|aarch64) ASSET="apple-mail-mcp-darwin-arm64" ;;
  x86_64)        ASSET="apple-mail-mcp-darwin-amd64" ;;
  *)             echo "Unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

mkdir -p "${BIN_DIR}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

BASE_URL="https://github.com/${REPO}/releases/latest/download"
echo "Downloading ${ASSET} from ${REPO}..."
curl -fsSL "${BASE_URL}/${ASSET}" -o "${TMP_DIR}/${ASSET}"
curl -fsSL "${BASE_URL}/SHA256SUMS" -o "${TMP_DIR}/SHA256SUMS"

(
  cd "${TMP_DIR}"
  grep "  ${ASSET}\$" SHA256SUMS | shasum -a 256 -c -
)

install -m 0755 "${TMP_DIR}/${ASSET}" "${BIN_PATH}"
echo "Installed ${BIN_PATH}"
