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
    CHECKSUM_URL="${LATEST_URL}/${BINARY}.sha256"

    echo "Detected: ${TARGET_OS}/${TARGET_ARCH}"
    echo "Downloading ${URL} ..."

    mkdir -p "$INSTALL_DIR"

    TMP_FILE="$(mktemp)"
    TMP_CHECKSUM="$(mktemp)"
    cleanup_tmp() {
        rm -f "$TMP_FILE" "$TMP_CHECKSUM"
    }
    trap cleanup_tmp EXIT INT TERM

    download_url "$URL" "$TMP_FILE"

    # Verify SHA256 checksum before touching the installed binary.
    echo "Verifying checksum ..."
    download_url "$CHECKSUM_URL" "$TMP_CHECKSUM"
    EXPECTED_HASH="$(cat "$TMP_CHECKSUM")"

    if command -v sha256sum > /dev/null 2>&1; then
        ACTUAL_HASH="$(sha256sum "$TMP_FILE" | awk '{print $1}')"
    elif command -v shasum > /dev/null 2>&1; then
        ACTUAL_HASH="$(shasum -a 256 "$TMP_FILE" | awk '{print $1}')"
    else
        echo "WARNING: sha256sum/shasum not found; skipping checksum verification." >&2
        ACTUAL_HASH="$EXPECTED_HASH"
    fi

    if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
        echo "Checksum mismatch." >&2
        echo "  Expected: $EXPECTED_HASH" >&2
        echo "  Got:      $ACTUAL_HASH" >&2
        exit 1
    fi
    echo "Checksum verified."

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
    echo "Running: orca --help"
    "${INSTALL_DIR}/${BINARY_NAME}" --help
}

main
