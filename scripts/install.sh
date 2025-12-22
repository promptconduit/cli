#!/bin/bash
#
# PromptConduit CLI Installer
#
# Usage:
#   curl -fsSL https://promptconduit.dev/install | bash
#   curl -fsSL https://promptconduit.dev/install | bash -s -- YOUR_API_KEY
#
# Environment variables:
#   PROMPTCONDUIT_VERSION - Install a specific version (default: latest)
#   PROMPTCONDUIT_INSTALL_DIR - Installation directory (default: /usr/local/bin)
#

set -e

# Configuration
REPO="promptconduit/cli"
BINARY_NAME="promptconduit"
DEFAULT_INSTALL_DIR="/usr/local/bin"
GITHUB_API="https://api.github.com"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Detect OS and architecture
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux*)  os="linux" ;;
        Darwin*) os="darwin" ;;
        MINGW*|MSYS*|CYGWIN*) os="windows" ;;
        *) error "Unsupported operating system: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}_${arch}"
}

# Get the latest version from GitHub
get_latest_version() {
    local version
    version=$(curl -sS "${GITHUB_API}/repos/${REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"v([^"]+)".*/\1/')

    if [ -z "$version" ]; then
        error "Failed to get latest version from GitHub"
    fi

    echo "$version"
}

# Download and install the binary
install_binary() {
    local version="$1"
    local platform="$2"
    local install_dir="$3"

    local ext="tar.gz"
    if [[ "$platform" == windows_* ]]; then
        ext="zip"
    fi

    local filename="${BINARY_NAME}_${version}_${platform}.${ext}"
    local download_url="https://github.com/${REPO}/releases/download/v${version}/${filename}"

    info "Downloading ${BINARY_NAME} v${version} for ${platform}..."

    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf $tmp_dir" EXIT

    if ! curl -fsSL "$download_url" -o "${tmp_dir}/${filename}"; then
        error "Failed to download from ${download_url}"
    fi

    info "Extracting..."
    cd "$tmp_dir"

    if [[ "$ext" == "tar.gz" ]]; then
        tar -xzf "$filename"
    else
        unzip -q "$filename"
    fi

    # Install the binary
    local binary="${BINARY_NAME}"
    if [[ "$platform" == windows_* ]]; then
        binary="${BINARY_NAME}.exe"
    fi

    info "Installing to ${install_dir}/${binary}..."

    # Check if we need sudo
    if [ -w "$install_dir" ]; then
        mv "$binary" "${install_dir}/"
        chmod +x "${install_dir}/${binary}"
    else
        warn "Requires sudo to install to ${install_dir}"
        sudo mv "$binary" "${install_dir}/"
        sudo chmod +x "${install_dir}/${binary}"
    fi

    info "Successfully installed ${BINARY_NAME} v${version} to ${install_dir}/${binary}"
}

# Configure API key if provided
configure_api_key() {
    local api_key="$1"

    if [ -z "$api_key" ]; then
        return
    fi

    info "API key provided. Add this to your shell profile:"
    echo ""
    echo "  export PROMPTCONDUIT_API_KEY=\"${api_key}\""
    echo ""

    # Try to detect shell and config file
    local shell_config=""
    if [ -n "$ZSH_VERSION" ] || [ "$SHELL" = "/bin/zsh" ]; then
        shell_config="$HOME/.zshrc"
    elif [ -n "$BASH_VERSION" ] || [ "$SHELL" = "/bin/bash" ]; then
        if [ -f "$HOME/.bash_profile" ]; then
            shell_config="$HOME/.bash_profile"
        else
            shell_config="$HOME/.bashrc"
        fi
    fi

    if [ -n "$shell_config" ]; then
        read -p "Add to ${shell_config}? [y/N] " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            echo "" >> "$shell_config"
            echo "# PromptConduit API Key" >> "$shell_config"
            echo "export PROMPTCONDUIT_API_KEY=\"${api_key}\"" >> "$shell_config"
            info "Added to ${shell_config}. Run 'source ${shell_config}' or restart your terminal."
        fi
    fi
}

main() {
    local api_key="${1:-}"
    local version="${PROMPTCONDUIT_VERSION:-}"
    local install_dir="${PROMPTCONDUIT_INSTALL_DIR:-$DEFAULT_INSTALL_DIR}"

    echo ""
    echo "  PromptConduit CLI Installer"
    echo "  ============================"
    echo ""

    # Detect platform
    local platform
    platform=$(detect_platform)
    info "Detected platform: ${platform}"

    # Get version
    if [ -z "$version" ]; then
        info "Fetching latest version..."
        version=$(get_latest_version)
    fi
    info "Version: ${version}"

    # Create install directory if needed
    if [ ! -d "$install_dir" ]; then
        warn "Creating install directory: ${install_dir}"
        sudo mkdir -p "$install_dir"
    fi

    # Install
    install_binary "$version" "$platform" "$install_dir"

    # Configure API key if provided
    configure_api_key "$api_key"

    echo ""
    info "Installation complete!"
    echo ""
    echo "  Next steps:"
    echo "    1. Set your API key: export PROMPTCONDUIT_API_KEY=\"your-key\""
    echo "    2. Install hooks:    promptconduit install claude-code"
    echo "    3. Check status:     promptconduit status"
    echo ""
}

main "$@"
