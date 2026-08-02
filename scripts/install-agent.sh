#!/bin/sh
set -eu

NETHERA_ENV="${NETHERA_ENV:-prod}"
NETHERA_API_URL="${NETHERA_API_URL:-https://api.nethera.io}"
NETHERA_DOWNLOADS_BASE_URL="${NETHERA_DOWNLOADS_BASE_URL:-https://get.nethera.io}"
DEFAULT_DOWNLOAD_BASE="${NETHERA_DOWNLOADS_BASE_URL%/}/releases/agent/latest"
ROOT_PREFIX="${NETHERA_AGENT_INSTALL_ROOT:-}"
INSTALL_DIR="${ROOT_PREFIX}/usr/local/bin"
AGENT_BIN="$INSTALL_DIR/nethera-agent"
INSTALL_VERSION=""

host_path() {
  path="$1"
  printf '%s' "${ROOT_PREFIX}${path}"
}

usage() {
  cat >&2 <<'USAGE'
Usage: install-agent.sh [--version <version>]

Environment:
  NETHERA_ENV             Environment written to /etc/nethera/agent.env.
  NETHERA_API_URL         API URL written to /etc/nethera/agent.env.
  NETHERA_DOWNLOADS_BASE_URL  Downloads URL written to /etc/nethera/agent.env.
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
      INSTALL_VERSION="$2"
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

require_root() {
  if [ -n "$ROOT_PREFIX" ]; then
    return
  fi
  if [ "$(id -u)" -ne 0 ]; then
    echo "The Nethera agent installer must be run as root." >&2
    echo "Try:" >&2
    echo "  curl -fsSL https://get.nethera.io/agent | sudo sh" >&2
    exit 1
  fi
}

detect_platform() {
  uname_s="$(uname -s 2>/dev/null || true)"
  case "$uname_s" in
    Linux*) OS="linux" ;;
    *)
      echo "The Nethera agent currently supports Linux only." >&2
      echo "Install the agent on a Linux machine you want to deploy apps to." >&2
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
}

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

verify_checksum() {
  binary_path="$1"
  checksums_path="$2"
  expected="$(awk -v artifact="$ARTIFACT" '$2 == artifact {print $1}' "$checksums_path" | head -n 1)"
  if [ -z "$expected" ]; then
    echo "Checksum for $ARTIFACT was not found in checksums.txt." >&2
    exit 1
  fi
  if ! command -v sha256sum >/dev/null 2>&1; then
    echo "Warning: could not verify checksum because sha256sum was not found." >&2
    return 0
  fi
  actual="$(sha256sum "$binary_path" | awk '{print $1}')"
  if [ "$actual" != "$expected" ]; then
    echo "Checksum verification failed for $ARTIFACT." >&2
    exit 1
  fi
}

load_os_release() {
  os_release_path="$(host_path /etc/os-release)"
  if [ ! -r "$os_release_path" ]; then
    echo "Could not read /etc/os-release." >&2
    echo "Install Docker, Docker Compose, and WireGuard manually, then install the agent binary." >&2
    exit 1
  fi
  # shellcheck disable=SC1091
  . "$os_release_path"
  DISTRO_ID="${ID:-}"
  DISTRO_ID_LIKE="${ID_LIKE:-}"
  DISTRO_CODENAME="${VERSION_CODENAME:-${UBUNTU_CODENAME:-}}"
  DISTRO_VERSION_ID="${VERSION_ID:-}"
  case "$DISTRO_ID" in
    debian|ubuntu)
      DISTRO_FAMILY="debian"
      ;;
    fedora)
      DISTRO_FAMILY="fedora"
      ;;
    rhel|rocky|almalinux|centos)
      DISTRO_FAMILY="rhel"
      ;;
    *)
      case " $DISTRO_ID_LIKE " in
        *" debian "*)
          DISTRO_FAMILY="debian"
          ;;
        *" fedora "*)
          DISTRO_FAMILY="fedora"
          ;;
        *" rhel "*)
          DISTRO_FAMILY="rhel"
          ;;
        *)
          echo "Unsupported Linux distribution: ${DISTRO_ID:-unknown}." >&2
          echo "This installer supports Debian, Ubuntu, Fedora, Rocky Linux, AlmaLinux, CentOS, and RHEL-like systems." >&2
          echo "Install Docker, Docker Compose, and WireGuard manually, then install the agent binary." >&2
          exit 1
          ;;
      esac
      ;;
  esac
  if [ "$DISTRO_FAMILY" = "debian" ] && [ -z "$DISTRO_CODENAME" ]; then
    echo "Could not detect Debian/Ubuntu release codename." >&2
    exit 1
  fi
  if [ "$DISTRO_FAMILY" = "rhel" ]; then
    RHEL_MAJOR="${DISTRO_VERSION_ID%%.*}"
    if [ -z "$RHEL_MAJOR" ]; then
      echo "Could not detect RHEL-compatible major version." >&2
      exit 1
    fi
  fi
}

apt_install_prerequisites() {
  apt-get update
  apt-get install -y ca-certificates curl gnupg
}

detect_debian_docker_conflicts() {
  conflicts=""
  for package in docker.io docker-doc docker-compose podman-docker containerd runc; do
    if dpkg-query -W -f='${Status}' "$package" 2>/dev/null | grep -q "install ok installed"; then
      conflicts="$conflicts $package"
    fi
  done
  if [ -n "$conflicts" ]; then
    echo "Conflicting Docker packages are installed:$conflicts" >&2
    echo "Remove or migrate them manually before installing Docker Engine from Docker's official repository." >&2
    exit 1
  fi
}

install_docker_apt_repo() {
  apt_install_prerequisites
  install -m 0755 -d "$(host_path /etc/apt/keyrings)"
  download "https://download.docker.com/linux/${DISTRO_ID}/gpg" "$(host_path /etc/apt/keyrings/docker.asc)"
  chmod a+r "$(host_path /etc/apt/keyrings/docker.asc)"
  dpkg_arch="$(dpkg --print-architecture)"
  mkdir -p "$(host_path /etc/apt/sources.list.d)"
  echo "deb [arch=${dpkg_arch} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/${DISTRO_ID} ${DISTRO_CODENAME} stable" >"$(host_path /etc/apt/sources.list.d/docker.list)"
  apt-get update
}

install_docker_apt_packages() {
  detect_debian_docker_conflicts
  install_docker_apt_repo
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

dnf_cmd() {
  if command -v dnf >/dev/null 2>&1; then
    echo "dnf"
  elif command -v yum >/dev/null 2>&1; then
    echo "yum"
  else
    echo "dnf/yum was not found. Install Docker, Docker Compose, and WireGuard manually, then rerun the installer." >&2
    exit 1
  fi
}

dnf_install_prerequisites() {
  pm="$(dnf_cmd)"
  "$pm" install -y ca-certificates curl dnf-plugins-core
}

install_docker_dnf_repo() {
  dnf_install_prerequisites
  pm="$(dnf_cmd)"
  case "$DISTRO_FAMILY" in
    fedora)
      repo_url="https://download.docker.com/linux/fedora/docker-ce.repo"
      ;;
    rhel)
      repo_url="https://download.docker.com/linux/centos/docker-ce.repo"
      ;;
    *)
      echo "Internal error: dnf Docker repo requested for $DISTRO_FAMILY." >&2
      exit 1
      ;;
  esac
  if "$pm" config-manager --help 2>&1 | grep -q -- '--add-repo'; then
    "$pm" config-manager --add-repo "$repo_url"
  else
    mkdir -p "$(host_path /etc/yum.repos.d)"
    download "$repo_url" "$(host_path /etc/yum.repos.d/docker-ce.repo)"
  fi
}

install_docker_dnf_packages() {
  install_docker_dnf_repo
  pm="$(dnf_cmd)"
  "$pm" install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

install_docker_packages() {
  case "$DISTRO_FAMILY" in
    debian) install_docker_apt_packages ;;
    fedora|rhel) install_docker_dnf_packages ;;
    *)
      echo "Unsupported Linux distribution family: $DISTRO_FAMILY." >&2
      exit 1
      ;;
  esac
}

docker_engine_available() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}

prompt_for_docker_engine() {
  cat >&2 <<'EOF'
Docker is installed, but the Docker engine is not responding.

Start Docker Engine, then Nethera can deploy apps to this machine.

On Linux:
  sudo systemctl start docker

On Docker Desktop / WSL:
  Open Docker Desktop and wait until it says Docker is running.
EOF

  if [ -r /dev/tty ] && printf '\nPress Enter after Docker is running, or type "skip" to continue for now: ' >/dev/tty 2>/dev/null; then
    if read -r docker_wait_answer </dev/tty; then
      case "$docker_wait_answer" in
        skip|SKIP|s|S)
          echo "Continuing without a reachable Docker engine. Deploys will wait until Docker is running." >&2
          return 0
          ;;
      esac
      if docker_engine_available; then
        echo "Docker Engine is running."
        return 0
      fi
    fi
  fi

  echo "Docker Engine is still not reachable. Deploys will wait until Docker is running." >&2
}

install_docker_if_missing() {
  if command -v docker >/dev/null 2>&1; then
    echo "Docker CLI is already installed."
  else
    echo "Installing Docker Engine..."
    install_docker_packages
  fi

  if docker compose version >/dev/null 2>&1; then
    echo "Docker Compose plugin is already installed."
  else
    echo "Installing Docker Compose plugin..."
    case "$DISTRO_FAMILY" in
      debian)
        install_docker_apt_repo
        apt-get install -y docker-compose-plugin
        ;;
      fedora|rhel)
        install_docker_dnf_repo
        "$(dnf_cmd)" install -y docker-compose-plugin
        ;;
    esac
  fi

  if command -v systemctl >/dev/null 2>&1; then
    systemctl enable --now docker >/dev/null 2>&1 || true
  fi

  if ! docker_engine_available; then
    prompt_for_docker_engine
  fi
}

install_wireguard_if_missing() {
  if command -v wg >/dev/null 2>&1; then
    echo "WireGuard tools are already installed."
  else
    echo "Installing WireGuard tools..."
    case "$DISTRO_FAMILY" in
      debian)
        apt-get update
        apt-get install -y wireguard
        ;;
      fedora)
        "$(dnf_cmd)" install -y wireguard-tools
        ;;
      rhel)
        "$(dnf_cmd)" install -y wireguard-tools
        ;;
      *)
        echo "Unsupported Linux distribution family: $DISTRO_FAMILY." >&2
        exit 1
        ;;
    esac
  fi
}

create_directories() {
  mkdir -p "$(host_path /etc/nethera)" "$(host_path /var/lib/nethera)" "$(host_path /var/log/nethera)"
  chmod 700 "$(host_path /etc/nethera)"
  chmod 700 "$(host_path /var/lib/nethera)"
  chmod 755 "$(host_path /var/log/nethera)"
  chown -R root:root "$(host_path /etc/nethera)" "$(host_path /var/lib/nethera)" "$(host_path /var/log/nethera)"
}

install_agent_binary() {
  echo "Downloading Nethera agent for ${OS}-${ARCH}..."
  download "$URL" "$tmp_dir/nethera-agent"
  if download "$CHECKSUM_URL" "$tmp_dir/checksums.txt"; then
    verify_checksum "$tmp_dir/nethera-agent" "$tmp_dir/checksums.txt"
  else
    echo "Warning: could not download checksums.txt; installing without checksum verification." >&2
  fi
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$tmp_dir/nethera-agent" "$AGENT_BIN"
  if ! "$AGENT_BIN" --version; then
    echo "Warning: installed agent did not report a version." >&2
  fi
}

write_agent_env() {
  agent_env_path="$(host_path /etc/nethera/agent.env)"
  preserved_env="$tmp_dir/agent.env.preserved"
  if [ -f "$agent_env_path" ]; then
    grep -v -E '^(NETHERA_ENV|NETHERA_API_URL|NETHERA_DOWNLOADS_BASE_URL|NETHERA_AGENT_STATE_DIR|NETHERA_AGENT_CONFIG_DIR|NETHERA_AGENT_UPDATE_CHANNEL)=' "$agent_env_path" >"$preserved_env" || true
    echo "Refreshing /etc/nethera/agent.env."
  else
    : >"$preserved_env"
  fi
  {
    cat "$preserved_env"
    cat <<EOF
NETHERA_ENV=$NETHERA_ENV
NETHERA_API_URL=$NETHERA_API_URL
NETHERA_DOWNLOADS_BASE_URL=$NETHERA_DOWNLOADS_BASE_URL
NETHERA_AGENT_STATE_DIR=/var/lib/nethera
NETHERA_AGENT_CONFIG_DIR=/etc/nethera
NETHERA_AGENT_UPDATE_CHANNEL=stable
EOF
  } >"$agent_env_path"
  chmod 600 "$agent_env_path"
  chown root:root "$agent_env_path"
}

write_systemd_service() {
  mkdir -p "$(host_path /etc/systemd/system)"
  cat >"$(host_path /etc/systemd/system/nethera-agent.service)" <<'EOF'
[Unit]
Description=Nethera Agent
Documentation=https://nethera.io
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
EnvironmentFile=-/etc/nethera/agent.env
ExecStart=/usr/local/bin/nethera-agent --backend ${NETHERA_API_URL} --config /etc/nethera/machine.json
Restart=always
RestartSec=5
KillSignal=SIGTERM
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF
}

reload_systemd_service() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "systemctl was not found. Install systemd or start /usr/local/bin/nethera-agent manually." >&2
    exit 1
  fi
  systemctl daemon-reload
  systemctl enable nethera-agent
}

start_service() {
  reload_systemd_service
  systemctl restart nethera-agent
  if ! systemctl is-active --quiet nethera-agent; then
    echo "Nethera agent service failed to start." >&2
    echo "Check logs with:" >&2
    echo "  sudo journalctl -u nethera-agent -n 100 --no-pager" >&2
    exit 1
  fi
}

machine_is_paired() {
  machine_config_path="$(host_path /etc/nethera/machine.json)"
  [ -s "$machine_config_path" ] || return 1
  grep -q '"machineId"[[:space:]]*:[[:space:]]*"[^"]' "$machine_config_path" || return 1
  grep -q '"machineToken"[[:space:]]*:[[:space:]]*"[^"]' "$machine_config_path" || return 1
  if grep -q '"environment"[[:space:]]*:' "$machine_config_path" &&
    ! grep -q "\"environment\"[[:space:]]*:[[:space:]]*\"$NETHERA_ENV\"" "$machine_config_path"; then
    existing_env="$(sed -n 's/.*"environment"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$machine_config_path" | head -n 1)"
    echo "Existing machine credentials are for ${existing_env:-another environment}, but this installer is for $NETHERA_ENV."
    return 1
  fi
  return 0
}

prompt_start_pairing() {
  if machine_is_paired; then
    echo "Existing machine pairing preserved."
    return
  fi

  echo
  echo "This machine is not paired with Nethera yet."
  if [ ! -r /dev/tty ] || ! printf 'Start the pairing flow now? [Y/n]: ' >/dev/tty 2>/dev/null; then
    echo "To start pairing later, run:"
    echo "  sudo NETHERA_ENV=\"$NETHERA_ENV\" NETHERA_API_URL=\"$NETHERA_API_URL\" /usr/local/bin/nethera-agent enroll --backend \"$NETHERA_API_URL\" --config /etc/nethera/machine.json"
    return
  fi

  if ! read -r start_pairing </dev/tty; then
    echo
    echo "To start pairing later, run:"
    echo "  sudo NETHERA_ENV=\"$NETHERA_ENV\" NETHERA_API_URL=\"$NETHERA_API_URL\" /usr/local/bin/nethera-agent enroll --backend \"$NETHERA_API_URL\" --config /etc/nethera/machine.json"
    return
  fi
  case "${start_pairing:-Y}" in
    y|Y|yes|YES)
      echo
      echo "Starting pairing. The pairing code is shown below."
      echo "Run the displayed neth machine pair command from your signed-in laptop shell."
      echo
      machine_config_path="$(host_path /etc/nethera/machine.json)"
      if NETHERA_ENV="$NETHERA_ENV" NETHERA_API_URL="$NETHERA_API_URL" NETHERA_DOWNLOADS_BASE_URL="$NETHERA_DOWNLOADS_BASE_URL" "$AGENT_BIN" enroll --backend "$NETHERA_API_URL" --config "$machine_config_path" --timeout 10m; then
        if ! machine_is_paired; then
          echo "Pairing completed, but machine credentials were not written to $machine_config_path." >&2
          echo "To try again, run:" >&2
          echo "  sudo NETHERA_ENV=\"$NETHERA_ENV\" NETHERA_API_URL=\"$NETHERA_API_URL\" /usr/local/bin/nethera-agent enroll --backend \"$NETHERA_API_URL\" --config /etc/nethera/machine.json" >&2
          exit 1
        fi
      else
        echo "Pairing did not complete." >&2
        echo "To try again, run:" >&2
        echo "  sudo NETHERA_ENV=\"$NETHERA_ENV\" NETHERA_API_URL=\"$NETHERA_API_URL\" /usr/local/bin/nethera-agent enroll --backend \"$NETHERA_API_URL\" --config /etc/nethera/machine.json" >&2
      fi
      ;;
    *)
      echo "Skipping pairing for now."
      echo "To start pairing later, run:"
      echo "  sudo NETHERA_ENV=\"$NETHERA_ENV\" NETHERA_API_URL=\"$NETHERA_API_URL\" /usr/local/bin/nethera-agent enroll --backend \"$NETHERA_API_URL\" --config /etc/nethera/machine.json"
      ;;
  esac
}

require_root
detect_platform
load_os_release

ARTIFACT="nethera-agent-${OS}-${ARCH}"
if [ -n "$INSTALL_VERSION" ]; then
  case "$INSTALL_VERSION" in
    v*|local-*) VERSION_PATH="$INSTALL_VERSION" ;;
    *) VERSION_PATH="v$INSTALL_VERSION" ;;
  esac
  DOWNLOAD_BASE="${NETHERA_DOWNLOAD_BASE:-${NETHERA_DOWNLOADS_BASE_URL%/}/releases/agent/${VERSION_PATH}}"
else
  DOWNLOAD_BASE="${NETHERA_DOWNLOAD_BASE:-$DEFAULT_DOWNLOAD_BASE}"
fi
URL="${DOWNLOAD_BASE}/${ARTIFACT}"
CHECKSUM_URL="${DOWNLOAD_BASE}/checksums.txt"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT INT HUP TERM

install_docker_if_missing
install_wireguard_if_missing
create_directories
install_agent_binary
write_agent_env
write_systemd_service
reload_systemd_service
prompt_start_pairing
start_service

echo
if [ "$NETHERA_ENV" = "prod" ]; then
  echo "Nethera agent installed and started."
else
  echo "Nethera agent installed and started for $NETHERA_ENV."
  echo "This agent talks to:"
  echo "  $NETHERA_API_URL"
fi
echo "Check status:"
echo "  sudo systemctl status nethera-agent"
echo "View logs:"
echo "  sudo journalctl -u nethera-agent -f"
