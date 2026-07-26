#!/bin/sh
set -eu

NETHERA_ENV="${NETHERA_ENV:-prod}"
NETHERA_API_URL="${NETHERA_API_URL:-https://api.nethera.io}"
NETHERA_DOWNLOADS_BASE_URL="${NETHERA_DOWNLOADS_BASE_URL:-https://get.nethera.io}"
DEFAULT_DOWNLOAD_BASE="${NETHERA_DOWNLOADS_BASE_URL%/}/releases/cli/latest"
DEFAULT_INSTALL_DIR="/usr/local/bin"
INSTALL_DIR="${INSTALL_DIR:-}"
VERSION=""

usage() {
  cat >&2 <<'USAGE'
Usage: install-cli.sh [--version <version>] [--install-dir <path>]

Environment:
  INSTALL_DIR             Override install directory.
  NETHERA_ENV             Environment written to ~/.config/nethera/config.json.
  NETHERA_API_URL         API URL written to ~/.config/nethera/config.json.
  NETHERA_DOWNLOADS_BASE_URL  Downloads base URL written to ~/.config/nethera/config.json.
  NETHERA_DOWNLOAD_BASE   Override binary download base URL.
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      if [ "$#" -lt 2 ]; then
        echo "Missing value for --version" >&2
        exit 1
      fi
      VERSION="$2"
      shift 2
      ;;
    --install-dir)
      if [ "$#" -lt 2 ]; then
        echo "Missing value for --install-dir" >&2
        exit 1
      fi
      INSTALL_DIR="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [ -z "$INSTALL_DIR" ]; then
  if [ -d "$DEFAULT_INSTALL_DIR" ] && [ -w "$DEFAULT_INSTALL_DIR" ]; then
    INSTALL_DIR="$DEFAULT_INSTALL_DIR"
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi

uname_s="$(uname -s 2>/dev/null || true)"
case "$uname_s" in
  Linux*) OS="linux" ;;
  Darwin*) OS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*|Windows*)
    echo "Windows is not supported by this installer yet." >&2
    echo "Use WSL, or download a Windows binary when available." >&2
    exit 1
    ;;
  *)
    echo "Unsupported operating system: ${uname_s:-unknown}" >&2
    exit 1
    ;;
esac

uname_m="$(uname -m 2>/dev/null || true)"
case "$uname_m" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: ${uname_m:-unknown}" >&2
    exit 1
    ;;
esac

ARTIFACT="neth-${OS}-${ARCH}"
if [ -n "$VERSION" ]; then
  case "$VERSION" in
    v*|local-*) VERSION_PATH="$VERSION" ;;
    *) VERSION_PATH="v$VERSION" ;;
  esac
  DOWNLOAD_BASE="${NETHERA_DOWNLOAD_BASE:-${NETHERA_DOWNLOADS_BASE_URL%/}/releases/cli/${VERSION_PATH}}"
else
  DOWNLOAD_BASE="${NETHERA_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
fi

URL="${DOWNLOAD_BASE}/${ARTIFACT}"
CHECKSUM_URL="${DOWNLOAD_BASE}/checksums.txt"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT HUP TERM

download() {
  url="$1"
  output="$2"
  if command -v curl >/dev/null 2>&1; then
    if ! curl -fsSL "$url" -o "$output"; then
      echo "Download failed: $url" >&2
      exit 1
    fi
  elif command -v wget >/dev/null 2>&1; then
    if ! wget -q "$url" -O "$output"; then
      echo "Download failed: $url" >&2
      exit 1
    fi
  else
    echo "This installer requires curl or wget." >&2
    exit 1
  fi
}

sha256_file() {
  file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
  else
    return 1
  fi
}

verify_checksum() {
  binary_path="$1"
  checksums_path="$2"
  expected="$(awk -v artifact="$ARTIFACT" '$2 == artifact {print $1}' "$checksums_path" | head -n 1)"
  if [ -z "$expected" ]; then
    echo "Checksum for $ARTIFACT was not found in checksums.txt." >&2
    exit 1
  fi
  if ! actual="$(sha256_file "$binary_path")"; then
    echo "Warning: could not verify checksum because no SHA-256 tool was found." >&2
    return 0
  fi
  if [ "$actual" != "$expected" ]; then
    echo "Checksum verification failed for $ARTIFACT." >&2
    exit 1
  fi
}

display_path() {
  path="$1"
  case "$path" in
    "$HOME"/*) printf '~/%s' "${path#"$HOME"/}" ;;
    *) printf '%s' "$path" ;;
  esac
}

shell_path_expr() {
  path="$1"
  case "$path" in
    "$HOME"/*) printf '$HOME/%s' "${path#"$HOME"/}" ;;
    *) printf '%s' "$path" ;;
  esac
}

path_contains_dir() {
  dir="$1"
  case ":$PATH:" in
    *":$dir:"*) return 0 ;;
    *) return 1 ;;
  esac
}

echo "Downloading Nethera CLI for ${OS}-${ARCH}..."
download "$URL" "$tmp_dir/neth"

if download "$CHECKSUM_URL" "$tmp_dir/checksums.txt"; then
  verify_checksum "$tmp_dir/neth" "$tmp_dir/checksums.txt"
else
  echo "Warning: could not download checksums.txt; installing without checksum verification." >&2
fi

chmod +x "$tmp_dir/neth"

if ! mkdir -p "$INSTALL_DIR" 2>/dev/null; then
  echo "Could not create $INSTALL_DIR." >&2
  echo "Try:" >&2
  echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sudo sh" >&2
  echo "or:" >&2
  echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sh -s -- --install-dir \"\$HOME/.local/bin\"" >&2
  exit 1
fi

if command -v install >/dev/null 2>&1; then
  if ! install -m 0755 "$tmp_dir/neth" "$INSTALL_DIR/neth" 2>/dev/null; then
    echo "Could not write to $INSTALL_DIR." >&2
    echo "Try:" >&2
    echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sudo sh" >&2
    echo "or:" >&2
    echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sh -s -- --install-dir \"\$HOME/.local/bin\"" >&2
    exit 1
  fi
else
  if ! mv "$tmp_dir/neth" "$INSTALL_DIR/neth" 2>/dev/null; then
    echo "Could not write to $INSTALL_DIR." >&2
    echo "Try:" >&2
    echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sudo sh" >&2
    echo "or:" >&2
    echo "  curl -fsSL ${NETHERA_DOWNLOADS_BASE_URL%/}/cli | sh -s -- --install-dir \"\$HOME/.local/bin\"" >&2
    exit 1
  fi
  chmod 0755 "$INSTALL_DIR/neth"
fi

echo "Nethera CLI installed to $(display_path "$INSTALL_DIR")/neth"

config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/nethera"
config_path="$config_dir/config.json"
mkdir -p "$config_dir"
cat >"$config_path" <<EOF
{
  "currentEnvironment": "$NETHERA_ENV",
  "environments": {
    "$NETHERA_ENV": {
      "apiUrl": "$NETHERA_API_URL",
      "downloadsBaseUrl": "$NETHERA_DOWNLOADS_BASE_URL"
    }
  }
}
EOF
chmod 0600 "$config_path"

"$INSTALL_DIR/neth" --version || true

if ! path_contains_dir "$INSTALL_DIR"; then
  echo
  echo "Installed neth to $(display_path "$INSTALL_DIR"), but that directory is not on your PATH."
  echo "Add this to your shell profile:"
  echo "  export PATH=\"$(shell_path_expr "$INSTALL_DIR"):\$PATH\""
fi

echo
if [ "$NETHERA_ENV" = "staging" ]; then
  echo "Installed Nethera CLI for staging."
  echo "This installer configures the neth CLI for the staging environment."
  echo "If you later install the production CLI, your default environment may change."
else
  echo "Nethera CLI installed successfully."
fi
echo "This CLI talks to:"
echo "$NETHERA_API_URL"
echo "Register or log in:"
echo "  neth login"
