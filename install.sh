#!/bin/sh
# Rhombus CLI installer for macOS and Linux
# Usage: curl -fsSL https://raw.githubusercontent.com/RhombusSystems/rhombus-cli/main/install.sh | sh

set -e

REPO="RhombusSystems/rhombus-cli"
INSTALL_DIR="${RHOMBUS_INSTALL_DIR:-/usr/local/bin}"

# Detect OS
OS="$(uname -s)"
case "$OS" in
    Linux*)  os="linux" ;;
    Darwin*) os="darwin" ;;
    *)       echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)  arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *)             echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest release tag
echo "Fetching latest release..."
version="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/')"
echo "Latest version: ${version}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

# On Linux, prefer native packages if package manager is available
if [ "$os" = "linux" ]; then
    if command -v dpkg >/dev/null 2>&1; then
        asset="rhombus-cli_${version}_${arch}.deb"
        url="https://github.com/${REPO}/releases/download/v${version}/${asset}"
        echo "Downloading ${asset}..."
        curl -fsSL "$url" -o "${tmpdir}/${asset}"
        echo "Installing .deb package..."
        sudo dpkg -i "${tmpdir}/${asset}"
        echo ""
        echo "Rhombus CLI v${version} installed via dpkg"
        echo "Run 'rhombus --help' to get started."
        exit 0
    elif command -v rpm >/dev/null 2>&1; then
        asset="rhombus-cli_${version}_${arch}.rpm"
        url="https://github.com/${REPO}/releases/download/v${version}/${asset}"
        echo "Downloading ${asset}..."
        curl -fsSL "$url" -o "${tmpdir}/${asset}"
        echo "Installing .rpm package..."
        sudo rpm -U "${tmpdir}/${asset}"
        echo ""
        echo "Rhombus CLI v${version} installed via rpm"
        echo "Run 'rhombus --help' to get started."
        exit 0
    fi
fi

# Fallback: download tarball and copy binary
asset="rhombus-cli_${version}_${os}_${arch}.tar.gz"
url="https://github.com/${REPO}/releases/download/v${version}/${asset}"

echo "Downloading ${asset}..."
curl -fsSL "$url" -o "${tmpdir}/${asset}"

echo "Extracting..."
tar -xzf "${tmpdir}/${asset}" -C "$tmpdir"

echo "Installing to ${INSTALL_DIR}/rhombus..."
if [ -w "$INSTALL_DIR" ]; then
    cp "${tmpdir}/rhombus" "${INSTALL_DIR}/rhombus"
    chmod +x "${INSTALL_DIR}/rhombus"
else
    sudo cp "${tmpdir}/rhombus" "${INSTALL_DIR}/rhombus"
    sudo chmod +x "${INSTALL_DIR}/rhombus"
fi

echo ""
echo "Rhombus CLI v${version} installed to ${INSTALL_DIR}/rhombus"
echo "Run 'rhombus --help' to get started."
