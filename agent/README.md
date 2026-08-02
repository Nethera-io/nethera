# Nethera Agent

This repository contains the Linux agent that runs on machines managed by Nethera.

The agent polls the Nethera backend for desired deployment state, reconciles Docker Compose on the target machine, reports service health, streams logs on request, and maintains the machine-side WireGuard configuration used by Nethera endpoints.

## Repository scope

This repo owns agent source code and tests only.

Public download scripts, CLI source, examples, and published downloadable release artifacts live in `Nethera-io/nethera`.

## Develop locally

```bash
go test ./...
go build -o nethera-agent .
sudo ./nethera-agent enroll --backend http://127.0.0.1:8081
sudo ./nethera-agent --backend http://127.0.0.1:8081
```

Set `NETHERA_ENV` and `NETHERA_API_URL` when testing against staging or production-like environments.

## Release flow

The ops repo builds agent binaries from this repository and uploads the downloadable artifacts to the public Nethera downloads release.

The public installer remains:

```bash
curl -fsSL https://get.nethera.io/agent | sudo sh
```
