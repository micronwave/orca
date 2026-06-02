#!/bin/sh
# install.sh — installs orca CLI for Mac and Linux.
# Usage: curl -fsSL https://raw.githubusercontent.com/micronwave/orca/main/install.sh | sh
set -eu

REPO="micronwave/orca"
RELEASES_URL="https://github.com/${REPO}/releases"
LATEST_URL="${RELEASES_URL}/latest/download"
INSTALL_DIR="${HOME}/.local/bin"
BINARY_NAME="orca"

# Colors and Icons — cleared when NO_COLOR is set or the terminal is dumb.
CYAN='\033[0;36m'
WHITE='\033[1;37m'
GREEN='\033[0;32m'
RED='\033[0;31m'
BOLD='\033[1m'
NC='\033[0m' # No Color

# Respect NO_COLOR (https://no-color.org/) and dumb terminals.
if [ -n "${NO_COLOR:-}" ] || [ "${TERM:-}" = "dumb" ]; then
    CYAN='' WHITE='' GREEN='' RED='' BOLD='' NC=''
fi

ICON_ORCA="≋"
ICON_PKG="❒"
ICON_CHECK="✓"
ICON_ERROR="✗"
ICON_ROCKET="✦"

show_banner() {
    printf "${CYAN}   .            _${NC}\n"
    printf "${CYAN}  - ${CYAN}_ ${CYAN}_  _  ${CYAN}( ) ${CYAN}_${NC}\n"
    printf "${CYAN}- ( ${WHITE}_ ${CYAN})( ${WHITE}_ ${CYAN})( ${WHITE}_ ${CYAN})${CYAN}| |${CYAN}( ${WHITE}_ ${CYAN})${NC}\n"
    printf "${CYAN}-  ${WHITE}(_ ${CYAN}) ${CYAN}| | ${CYAN}|  ${CYAN}_${CYAN}/| |${CYAN}| ${WHITE}_ ${CYAN}|${NC}\n"
    printf "${CYAN} - ${CYAN}( ${WHITE}_ ${CYAN}) ${CYAN}|_| ${CYAN}| ${WHITE}_ ${CYAN}| ${CYAN}|_|${CYAN}( ${WHITE}_ ${CYAN})${NC}\n"
    printf "  -  -   -   -   -   -\n"
    printf "\n"
}

status() {
    printf "${CYAN}${BOLD}[orca]${NC} %b\n" "$1"
}

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
    show_banner
    need_cmd uname

    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux*)  TARGET_OS="linux" ;;
        Darwin*) TARGET_OS="darwin" ;;
        *)
            status "${ICON_ERROR} ${RED}Unsupported OS: ${OS}${NC}" >&2
            echo "Visit ${RELEASES_URL} for a manual download." >&2
            exit 1
            ;;
    esac

    case "$ARCH" in
        x86_64|amd64)   TARGET_ARCH="amd64" ;;
        aarch64|arm64)  TARGET_ARCH="arm64" ;;
        *)
            status "${ICON_ERROR} ${RED}Unsupported architecture: ${ARCH}${NC}" >&2
            echo "Visit ${RELEASES_URL} for a manual download." >&2
            exit 1
            ;;
    esac

    BINARY="orca-${TARGET_OS}-${TARGET_ARCH}"
    URL="${LATEST_URL}/${BINARY}"
    CHECKSUM_URL="${LATEST_URL}/${BINARY}.sha256"

    status "Detected: ${WHITE}${TARGET_OS}/${TARGET_ARCH}${NC}"
    status "${ICON_PKG} Downloading ${WHITE}${BINARY}${NC} ..."

    mkdir -p "$INSTALL_DIR"

    TMP_FILE="$(mktemp)"
    TMP_CHECKSUM="$(mktemp)"
    cleanup_tmp() {
        rm -f "$TMP_FILE" "$TMP_CHECKSUM"
    }
    trap cleanup_tmp EXIT INT TERM

    download_url "$URL" "$TMP_FILE"

    # Verify SHA256 checksum before touching the installed binary.
    status "Verifying checksum ..."
    download_url "$CHECKSUM_URL" "$TMP_CHECKSUM"
    EXPECTED_HASH="$(cat "$TMP_CHECKSUM")"

    if command -v sha256sum > /dev/null 2>&1; then
        ACTUAL_HASH="$(sha256sum "$TMP_FILE" | awk '{print $1}')"
    elif command -v shasum > /dev/null 2>&1; then
        ACTUAL_HASH="$(shasum -a 256 "$TMP_FILE" | awk '{print $1}')"
    else
        status "${RED}WARNING: sha256sum/shasum not found; skipping checksum verification.${NC}" >&2
        ACTUAL_HASH="$EXPECTED_HASH"
    fi

    if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
        status "${ICON_ERROR} ${RED}Checksum mismatch.${NC}" >&2
        echo "  Expected: $EXPECTED_HASH" >&2
        echo "  Got:      $ACTUAL_HASH" >&2
        exit 1
    fi
    status "${ICON_CHECK} ${GREEN}Checksum verified.${NC}"

    chmod +x "$TMP_FILE"
    mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
    trap - EXIT INT TERM

    status "${ICON_CHECK} ${GREEN}Installed: ${WHITE}${INSTALL_DIR}/${BINARY_NAME}${NC}"

    # Warn if INSTALL_DIR is not on PATH.
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            echo ""
            status "${RED}${BOLD}WARNING: ${INSTALL_DIR} is not in your PATH.${NC}"
            echo "Add this line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
            echo "  export PATH=\"\${HOME}/.local/bin:\${PATH}\""
            ;;
    esac

    echo ""
    status "${ICON_ROCKET} ${GREEN}${BOLD}Installation complete!${NC}"
    echo "Running: orca --help"
    "${INSTALL_DIR}/${BINARY_NAME}" --help
}

main
