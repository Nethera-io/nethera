# Nethera

Deploy Docker Compose apps to machines you control and give selected services a public HTTPS URL.

Nethera is for home servers, GPU rigs, spare boxes, lab machines, and small fleets where you want the app to keep running on your hardware, but you do not want to configure static IPs, port forwarding, router rules, or a reverse proxy by hand.

```bash
curl -fsSL https://get.nethera.io/cli | sh
neth login
neth deploy
```

## What Nethera Does

Nethera separates deployment from traffic:

- the `neth` CLI reads `nethera.yml`, manages app secrets, pairs machines, deploys apps, streams logs, and copies files;
- the Nethera agent runs on your Linux machine and reconciles Docker Compose locally;
- Nethera edge routes public HTTPS traffic to the service ports you expose;
- endpoint auth can protect services with Nethera login or endpoint tokens.

Your containers, volumes, files, GPUs, and app data remain on machines you control.

## Start With an Example

Each example is a small, copyable Nethera app:

```bash
cd examples/open-webui
neth init
neth deploy
```

Available examples:

- `examples/open-webui`
- `examples/ollama`
- `examples/vllm`
- `examples/jupyter`
- `examples/comfyui`
- `examples/n8n`
- `examples/plausible`
- `examples/uptime-kuma`
- `examples/nocodb`
- `examples/docmost`
- `examples/ci-runner`
- `examples/immich`
- `examples/nextcloud`
- `examples/paperless-ngx`
- `examples/vaultwarden`
- `examples/home-assistant`
- `examples/jellyfin`

Full guides live at [nethera.io/docs/recipes](https://nethera.io/docs/recipes).

## Bring Your Own Compose File

If you already have Docker Compose, Nethera is a small extension rather than a new deployment format.

Add a `nethera:` block to the service you want reachable:

```yaml
appName: my-app

services:
  web:
    image: ghcr.io/acme/my-app:latest
    ports:
      - 3000
    nethera:
      public: 3000
      auth: login
```

Then deploy:

```bash
neth init
neth deploy
```

## Install the CLI

```bash
curl -fsSL https://get.nethera.io/cli | sh
```

Install a specific version:

```bash
curl -fsSL https://get.nethera.io/cli | sh -s -- --version 0.1.49
```

## Install the Agent

Run this on the Linux machine that should run your apps:

```bash
curl -fsSL https://get.nethera.io/agent | sudo sh
```

The installer installs Docker dependencies where supported, installs `nethera-agent` as a systemd service, and starts pairing when run interactively.

Supported installer targets:

- Debian and Ubuntu
- Fedora
- Rocky Linux, AlmaLinux, CentOS, and RHEL-like systems
- Linux `amd64` and `arm64`

Agent source now lives in `Nethera-io/nethera-agent`. This repository keeps the public agent installer and downloadable release artifacts.

## Repository Scope

This repository contains:

- `cli/` - the `neth` CLI
- `scripts/` - public install scripts for the CLI and agent
- `examples/` - copyable `nethera.yml` examples
- release/download artifact definitions for `get.nethera.io`

Separate repositories own the other product services:

- `nethera-agent` - Linux agent source
- `nethera-backend` - control plane API
- `nethera-edge` - edge router
- `nethera-frontend` - public site, docs, and dashboard
- `nethera-ops` - Terraform, deploy scripts, manifests, and runbooks

## Download Layout

The public downloads host serves stable install endpoints:

```text
https://get.nethera.io/cli
https://get.nethera.io/agent
```

Versioned artifacts are published under:

```text
https://get.nethera.io/releases/cli/v<version>/neth-linux-amd64
https://get.nethera.io/releases/cli/v<version>/neth-darwin-arm64
https://get.nethera.io/releases/agent/v<version>/nethera-agent-linux-amd64
https://get.nethera.io/releases/agent/v<version>/nethera-agent-linux-arm64
```

Each release directory includes `checksums.txt`.

## Local Development

Run CLI tests:

```bash
cd cli
go test ./...
```

Run the CLI against a local backend:

```bash
NETHERA_API_URL=http://127.0.0.1:8081 go run . login
```

Use the agent repo for local agent development.

## Docs

- Product docs: [nethera.io/docs](https://nethera.io/docs)
- `nethera.yml` reference: [nethera.io/docs/nethera-yml](https://nethera.io/docs/nethera-yml)
- Recipes: [nethera.io/docs/recipes](https://nethera.io/docs/recipes)
