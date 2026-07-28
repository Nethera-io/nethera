<div align="center">

# Nethera

**Deploy Docker Compose apps to machines you control. Get a public HTTPS URL without touching a router.**

[![CLI Release](https://img.shields.io/badge/cli-latest-blue)](https://nethera.io/docs)
[![Docs](https://img.shields.io/badge/docs-nethera.io%2Fdocs-black)](https://nethera.io/docs)
[![License](https://img.shields.io/badge/license-TBD-lightgrey)](#license)

[Docs](https://nethera.io/docs) · [Recipes](https://nethera.io/docs/recipes) · [nethera.yml reference](https://nethera.io/docs/nethera-yml)

</div>

---

Nethera is for home servers, GPU rigs, spare boxes, lab machines, and small fleets — anywhere you want an app to keep running on hardware you own, without configuring static IPs, port forwarding, router rules, or a reverse proxy by hand.

Install the CLI on your laptop, install the agent on the Linux machine that will run your apps, then deploy:

```bash
# on your laptop
curl -fsSL https://get.nethera.io/cli | sh
neth login

# on the machine that will run your apps
curl -fsSL https://get.nethera.io/agent | sudo sh

# back on your laptop, from a project directory
neth init
neth deploy
```

## Why Nethera

- **You already know Compose.** Nethera doesn't invent a new deployment format — it extends the `docker-compose.yml` you already have with a small `nethera:` block.
- **Your hardware, your data.** Containers, volumes, files, GPUs, and app data stay on machines you control. Nethera never takes your data off-box.
- **No networking chores.** No static IPs, no port forwarding, no router config, no manually wiring a reverse proxy or TLS certs.
- **Public URLs on demand.** Expose only the services you choose, and optionally require Nethera login or endpoint tokens to reach them.

## Core Concepts

Four terms come up throughout the docs:

- **Machine** — a Linux box you've paired with the `neth` agent. Pair one or many.
- **App** — one or more services described in a `nethera.yml`, deployed together to one or more machines.
- **Endpoint** — the public HTTPS URL Nethera creates for a service you've marked as public.
- **`nethera.yml`** — a `docker-compose.yml` with small additions: a few top-level fields naming the app and its deployment targets, and a `nethera:` block on any service you want reachable from outside your network. Everything else works the way it already does in Compose.

## Architecture

Nethera has two separate paths: one for deploying and reconciling apps, and one for serving public HTTPS requests. **The agent is part of the deploy path — it is not in the runtime request path.**

**Deploy control path.** `neth deploy` sends the app spec, managed files, and desired deployment target to the Nethera control plane, which stores desired state, validates the app, tracks revisions, stores secret references, and prepares endpoint routing and auth. The agent on your machine polls for desired state; when it receives a job, it fetches scoped deployment secrets, writes generated deployment files, binds public services to the machine's WireGuard address, and runs Docker Compose locally.

**Runtime request path.** Public requests go to Nethera Edge, not to the agent. The edge terminates TLS, matches the hostname, enforces endpoint auth, and picks a healthy backend for the service, then forwards the request over the WireGuard tunnel to the app port on your machine. Your Docker Compose app handles the request while its volumes, local databases, model caches, and files stay on the machine running the agent.

Why the split matters:

- The agent can be offline without traffic automatically going down.
- The edge uses service-port reachability to decide whether a backend can serve.
- No inbound public ports are required on your machine.
- Multi-machine endpoints can route across reachable service instances.
- Local Docker volumes and bind mounts are not replicated between machines.

## Quickstart

The normal first-deployment flow:

**1. Install the CLI** — on your laptop or dev machine (see [Installation](#installation) for pinning a version):

```bash
curl -fsSL https://get.nethera.io/cli | sh
```

**2. Install the agent** — on the Linux machine that will run your apps (see [Installation](#installation) for supported platforms):

```bash
curl -fsSL https://get.nethera.io/agent | sudo sh
```

The agent initiates pairing and prompts you to attach the machine to your Nethera workspace.

**3. Choose a starting point** — pick one:

- **Start with a recipe** — pick one from [recipes](https://nethera.io/docs/recipes), copy its example `nethera.yml` into a project directory, then run `neth init`. If the file already has app content, `neth init` preserves it and fills in the missing Nethera metadata (like deployment targets).
- **Start with `docker-compose.yml`** — if your project already has `docker-compose.yml` or `compose.yml`, keep it in the project directory; `neth init` can import it into `nethera.yml`. You may still need to replace unsupported local features such as `build:`, relative bind mounts, or `env_file`.
- **Start blank** — with no Compose file, `neth init` creates a small hello-world placeholder `nethera.yml` you can edit.

**4. Run `neth init`** from your project directory:

```bash
neth init
```

`neth init` writes `nethera.yml`, asks which paired machine or machines should be deployment targets, and uses the current directory name as the default app name.

**5. Review `nethera.yml`** — for most apps you'll edit the generated file so each service uses an image, and any public service has a `nethera:` block:

```yaml
appName: example-app
targets:
  - home-gpu
services:
  web:
    image: ghcr.io/acme/example-app:latest
    nethera:
      public: 3000
      auth: login
```

**6. Deploy** from the directory containing `nethera.yml`:

```bash
neth deploy
```

When the deploy completes, Nethera prints the public HTTPS endpoint.

## Examples

Each example is a small, copyable Nethera app you can deploy with `neth init && neth deploy`:

| Example | What it is |
|---|---|
| `examples/open-webui` | Chat UI for local LLMs |
| `examples/ollama` | Local model runner |
| `examples/vllm` | High-throughput LLM inference server |
| `examples/jupyter` | Notebook server |
| `examples/comfyui` | Node-based image generation |
| `examples/n8n` | Workflow automation |
| `examples/plausible` | Privacy-friendly analytics |
| `examples/uptime-kuma` | Uptime monitoring |
| `examples/nocodb` | No-code database UI |
| `examples/docmost` | Docs and wiki |
| `examples/ci-runner` | Self-hosted CI runner |
| `examples/immich` | Photo and video backup |
| `examples/nextcloud` | File sync and share |
| `examples/paperless-ngx` | Document management |
| `examples/vaultwarden` | Password manager |
| `examples/home-assistant` | Home automation |
| `examples/jellyfin` | Media server |

Full walkthroughs for each: [nethera.io/docs/recipes](https://nethera.io/docs/recipes).

## Bring Your Own Compose File

If you already run Docker Compose, adopting Nethera is a small diff, not a rewrite.

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

Then:

```bash
neth init
neth deploy
```

`public` exposes the port through Nethera edge; `auth: login` gates access behind Nethera login. See the [`nethera.yml` reference](https://nethera.io/docs/nethera-yml) for every option.

## Installation

### CLI

```bash
curl -fsSL https://get.nethera.io/cli | sh
```

Pin a specific version:

```bash
curl -fsSL https://get.nethera.io/cli | sh -s -- --version 0.1.49
```

### Agent

Run on the Linux machine that will host your apps:

```bash
curl -fsSL https://get.nethera.io/agent | sudo sh
```

The installer sets up Docker where supported, installs `nethera-agent` as a systemd service, and starts pairing when run interactively.

**Supported targets:**
- Debian, Ubuntu
- Fedora
- Rocky Linux, AlmaLinux, CentOS, and other RHEL-likes
- Linux `amd64` and `arm64`

## Downloads

Stable, always-current install endpoints:

```text
https://get.nethera.io/cli
https://get.nethera.io/agent
```

Versioned release artifacts:

```text
https://get.nethera.io/releases/cli/v<version>/neth-linux-amd64
https://get.nethera.io/releases/cli/v<version>/neth-darwin-arm64
https://get.nethera.io/releases/agent/v<version>/nethera-agent-linux-amd64
https://get.nethera.io/releases/agent/v<version>/nethera-agent-linux-arm64
```

Every release directory ships a `checksums.txt`.

## Repository Scope

This repository contains:

```text
scripts/    public install scripts for the CLI and agent
examples/   copyable nethera.yml examples
```

...plus the release/download artifact definitions served from `get.nethera.io`.

## Documentation

- Product docs — [nethera.io/docs](https://nethera.io/docs)
- `nethera.yml` reference — [nethera.io/docs/nethera-yml](https://nethera.io/docs/nethera-yml)
- Recipes — [nethera.io/docs/recipes](https://nethera.io/docs/recipes)

## License

<!-- Add your license badge/section here, e.g. MIT, Apache-2.0, or "Source-available" -->