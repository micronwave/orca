#!/bin/sh
# install.sh — installs orca CLI for Mac and Linux.
# Usage: curl -fsSL https://raw.githubusercontent.com/micronwave/orca/main/install.sh | sh
set -eu

REPO="micronwave/orca"
RELEASES_URL="https://github.com/${REPO}/releases"
LATEST_URL="${RELEASES_URL}/latest/download"
INSTALL_DIR="${HOME}/.local/bin"
BINARY_NAME="orca"

need_cmd() {
    if ! command -v "$1" > /dev/null 2>&1; then
        echo "Required command not found: $1" >&2
        exit 1
    fi
}

download_url() {
    URL="$1"
    DEST="$2"
    if command -v curl > /dev/null 2>&1; then
        curl -fsSL "$URL" -o "$DEST"
    elif command -v wget > /dev/null 2>&1; then
        wget -qO- "$URL" > "$DEST"
    else
        echo "Neither curl nor wget is available. Install one and retry." >&2
        exit 1
    fi
}

main() {
    need_cmd uname

    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux*)  TARGET_OS="linux" ;;
        Darwin*) TARGET_OS="darwin" ;;
        *)
            echo "Unsupported OS: ${OS}" >&2
            echo "Visit ${RELEASES_URL} for a manual download." >&2
            exit 1
            ;;
    esac

    case "$ARCH" in
        x86_64|amd64)   TARGET_ARCH="amd64" ;;
        aarch64|arm64)  TARGET_ARCH="arm64" ;;
        *)
            echo "Unsupported architecture: ${ARCH}" >&2
            echo "Visit ${RELEASES_URL} for a manual download." >&2
            exit 1
            ;;
    esac

    BINARY="orca-${TARGET_OS}-${TARGET_ARCH}"
    URL="${LATEST_URL}/${BINARY}"

    echo "Detected: ${TARGET_OS}/${TARGET_ARCH}"
    echo "Downloading ${URL} ..."

    mkdir -p "$INSTALL_DIR"

    TMP_FILE="$(mktemp)"
    cleanup_tmp() {
        rm -f "$TMP_FILE"
    }
    trap cleanup_tmp EXIT INT TERM

    download_url "$URL" "$TMP_FILE"
    chmod +x "$TMP_FILE"
    mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
    trap - EXIT INT TERM

    echo "Installed: ${INSTALL_DIR}/${BINARY_NAME}"

    # Warn if INSTALL_DIR is not on PATH.
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            echo ""
            echo "WARNING: ${INSTALL_DIR} is not in your PATH."
            echo "Add this line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
            echo "  export PATH=\"\${HOME}/.local/bin:\${PATH}\""
            ;;
    esac

    echo ""
    echo "Installation complete."
    echo "Running: orca init --help"
    "${INSTALL_DIR}/${BINARY_NAME}" init --help
}

main
