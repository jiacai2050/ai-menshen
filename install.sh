#!/bin/sh

set -e

REPO="jiacai2050/ai-menshen"
BINARY_NAME="ai-menshen"
GITHUB_URL="https://github.com/${REPO}"

# Default values
VERSION="latest"
INSTALL_DIR="${HOME}/.local/bin"

# Help message
usage() {
    echo "Usage: $0 [options]"
    echo "Options:"
    echo "  -v, --version <ver>      Release version (e.g. v1.0.0), default is latest"
    echo "  -d, --install-dir <dir>  Directory to install binary, default is ~/.local/bin"
    echo "  -h, --help               Show this help message"
    exit 1
}

# Parse arguments
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|-v)
            if [ -n "$2" ]; then
                VERSION="$2"
                shift 2
            else
                echo "Error: --version requires an argument"
                usage
            fi
            ;;
        --install-dir|-d)
            if [ -n "$2" ]; then
                INSTALL_DIR="$2"
                shift 2
            else
                echo "Error: --install-dir requires an argument"
                usage
            fi
            ;;
        --help|-h)
            usage
            ;;
        *)
            echo "Unknown argument: $1"
            usage
            ;;
    esac
done

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
    linux*)  OS="linux" ;;
    darwin*) OS="darwin" ;;
    *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect Architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="x86_64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Resolve 'latest' version if needed
if [ "$VERSION" = "latest" ]; then
    echo "Resolving latest version..."
    VERSION=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
fi

if [ -z "$VERSION" ]; then
    echo "Could not find version ${VERSION} for ${REPO}"
    exit 1
fi

# Use a temporary directory for download and extraction
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

FILE_NAME="${BINARY_NAME}_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="${GITHUB_URL}/releases/download/${VERSION}/${FILE_NAME}"

echo "Downloading ${BINARY_NAME} ${VERSION} for ${OS}/${ARCH}..."
curl -fL "$DOWNLOAD_URL" -o "${TMP_DIR}/${FILE_NAME}"

echo "Extracting..."
tar -xzf "${TMP_DIR}/${FILE_NAME}" -C "$TMP_DIR" "${BINARY_NAME}"

chmod +x "${TMP_DIR}/${BINARY_NAME}"

# Ensure install directory exists
mkdir -p "${INSTALL_DIR}"

if [ -w "${INSTALL_DIR}" ]; then
    mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/"
else
    echo "Need sudo permissions to move binary to ${INSTALL_DIR}"
    sudo mv "${TMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/"
fi

echo "Successfully installed ${BINARY_NAME} to ${INSTALL_DIR}/${BINARY_NAME}"

# Check if INSTALL_DIR is in PATH
case ":${PATH}:" in
    *:"${INSTALL_DIR}":*) ;;
    *) echo "Warning: ${INSTALL_DIR} is not in your PATH. You may need to add it to your shell profile." ;;
esac

"${INSTALL_DIR}/${BINARY_NAME}" -version
