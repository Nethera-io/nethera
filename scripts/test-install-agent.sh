#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"

fail() {
  echo "test-install-agent: $*" >&2
  exit 1
}

make_fake_checksums() {
  artifact="$1"
  binary_path="$2"
  checksums_path="$3"
  sha256sum "$binary_path" | awk -v artifact="$artifact" '{print $1 "  " artifact}' >"$checksums_path"
}

run_case() {
  name="$1"
  os_release="$2"
  expected_repo="$3"
  expected_wireguard_package="$4"

  work_dir="$(mktemp -d)"
  trap 'rm -rf "$work_dir"' EXIT INT HUP TERM
  fake_root="$work_dir/root"
  fake_bin="$work_dir/bin"
  download_base="$work_dir/downloads"
  log_file="$work_dir/commands.log"
  mkdir -p "$fake_root/etc" "$fake_bin" "$download_base"
  mkdir -p "$fake_root/etc/nethera"
  printf '%s\n' "$os_release" >"$fake_root/etc/os-release"
  cat >"$fake_root/etc/nethera/agent.env" <<'EOF'
NETHERA_ENV=prod
NETHERA_AGENT_CUSTOM_TEST=value
EOF

  for tool in id uname awk sha256sum install mkdir chmod cat cp grep head rm dirname basename pwd mktemp sed; do
    tool_path="$(command -v "$tool" || true)"
    [ -n "$tool_path" ] || fail "required test tool not found: $tool"
    ln -s "$tool_path" "$fake_bin/$tool"
  done

  cat >"$fake_bin/curl" <<'EOF'
#!/bin/sh
url=""
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o)
      output="$2"
      shift 2
      ;;
    -*)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done
  case "$url" in
  file://*) cp "${url#file://}" "$output" ;;
  https://get.staging.nethera.io/releases/agent/latest/*)
    cp "$FAKE_DOWNLOAD_DIR/$(basename "$url")" "$output"
    ;;
  https://download.docker.com/*/docker-ce.repo)
    printf '[docker-ce]\nname=Docker CE\nbaseurl=%s\n' "$url" >"$output"
    ;;
  https://download.docker.com/*/gpg)
    printf 'fake docker gpg\n' >"$output"
    ;;
  *)
    echo "unexpected curl url: $url" >&2
    exit 1
    ;;
esac
EOF
  chmod +x "$fake_bin/curl"

  cat >"$fake_bin/dnf" <<EOF
#!/bin/sh
printf 'dnf %s\n' "\$*" >>"$log_file"
if [ "\${1:-}" = "config-manager" ] && [ "\${2:-}" = "--help" ]; then
  echo "--add-repo"
  exit 0
fi
exit 0
EOF
  chmod +x "$fake_bin/dnf"

  cat >"$fake_bin/yum" <<EOF
#!/bin/sh
printf 'yum %s\n' "\$*" >>"$log_file"
exit 0
EOF
  chmod +x "$fake_bin/yum"

  cat >"$fake_bin/apt-get" <<EOF
#!/bin/sh
printf 'apt-get %s\n' "\$*" >>"$log_file"
exit 0
EOF
  chmod +x "$fake_bin/apt-get"

  cat >"$fake_bin/dpkg" <<'EOF'
#!/bin/sh
echo amd64
EOF
  chmod +x "$fake_bin/dpkg"

  cat >"$fake_bin/dpkg-query" <<'EOF'
#!/bin/sh
exit 1
EOF
  chmod +x "$fake_bin/dpkg-query"

  cat >"$fake_bin/docker" <<EOF
#!/bin/sh
printf 'docker %s\n' "\$*" >>"$log_file"
case "\$*" in
  "version") exit 1 ;;
  "compose version") exit 0 ;;
esac
exit 0
EOF
  chmod +x "$fake_bin/docker"

  cat >"$fake_bin/chown" <<EOF
#!/bin/sh
printf 'chown %s\n' "\$*" >>"$log_file"
exit 0
EOF
  chmod +x "$fake_bin/chown"

  cat >"$fake_bin/systemctl" <<EOF
#!/bin/sh
printf 'systemctl %s\n' "\$*" >>"$log_file"
exit 0
EOF
  chmod +x "$fake_bin/systemctl"

  agent_artifact="nethera-agent-linux-amd64"
  cat >"$download_base/$agent_artifact" <<'EOF'
#!/bin/sh
case "${1:-}" in
  --version)
    echo "nethera-agent test"
    ;;
  enroll)
    config_path=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --config)
          config_path="$2"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    [ -n "$config_path" ] || exit 1
    mkdir -p "$(dirname "$config_path")"
    printf '{"machineId":"test","machineToken":"test","environment":"staging"}\n' >"$config_path"
    ;;
esac
EOF
  chmod +x "$download_base/$agent_artifact"
  make_fake_checksums "$agent_artifact" "$download_base/$agent_artifact" "$download_base/checksums.txt"

  PATH="$fake_bin" \
    FAKE_DOWNLOAD_DIR="$download_base" \
    NETHERA_AGENT_INSTALL_ROOT="$fake_root" \
    NETHERA_ENV=staging \
    NETHERA_API_URL=https://api.staging.nethera.io \
    NETHERA_DOWNLOADS_BASE_URL=https://get.staging.nethera.io \
    "$ROOT_DIR/scripts/install-agent.sh" </dev/null >/tmp/nethera-install-agent-"$name".log 2>&1 || {
      cat /tmp/nethera-install-agent-"$name".log >&2
      fail "$name installer failed"
    }

  grep -q "$expected_repo" "$log_file" || {
    cat "$log_file" >&2
    fail "$name did not configure expected Docker repo"
  }
  grep -q "$expected_wireguard_package" "$log_file" || {
    cat "$log_file" >&2
    fail "$name did not install expected WireGuard package"
  }
  test -x "$fake_root/usr/local/bin/nethera-agent" || fail "$name did not install agent binary"
  grep -q 'NETHERA_ENV=staging' "$fake_root/etc/nethera/agent.env" || fail "$name did not write agent env"
  grep -q 'NETHERA_AGENT_CONFIG_DIR=/etc/nethera' "$fake_root/etc/nethera/agent.env" || fail "$name did not write agent config dir"
  grep -q 'NETHERA_AGENT_CUSTOM_TEST=value' "$fake_root/etc/nethera/agent.env" || fail "$name did not preserve custom agent env"
  ! grep -q 'NETHERA_ENV=prod' "$fake_root/etc/nethera/agent.env" || fail "$name kept stale agent env"
  grep -q 'ExecStart=/usr/local/bin/nethera-agent --backend ${NETHERA_API_URL} --config /etc/nethera/machine.json' "$fake_root/etc/systemd/system/nethera-agent.service" ||
    fail "$name did not write explicit machine config path"

  rm -rf "$work_dir"
  trap - EXIT INT HUP TERM
}

run_case "fedora" \
  'ID=fedora
VERSION_ID=41' \
  'https://download.docker.com/linux/fedora/docker-ce.repo' \
  'install -y wireguard-tools'

run_case "rocky" \
  'ID=almalinux
VERSION="9.8 (Olive Jaguar)"
VERSION_ID=9.8
ID_LIKE="rhel centos fedora"' \
  'https://download.docker.com/linux/centos/docker-ce.repo' \
  'install -y wireguard-tools'

echo "install-agent distro tests passed"
