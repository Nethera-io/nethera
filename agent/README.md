# Nethera Agent

This directory contains the Linux agent that runs on machines managed by Nethera.

The agent polls the Nethera backend for desired deployment state, reconciles Docker Compose on the target machine, reports service health, streams logs on request, and maintains the machine-side WireGuard configuration used by Nethera endpoints.

## Develop locally

```bash
go test ./...
go build -o nethera-agent .
sudo ./nethera-agent enroll --backend http://127.0.0.1:8081
sudo ./nethera-agent --backend http://127.0.0.1:8081
```

Set `NETHERA_ENV` and `NETHERA_API_URL` when testing against another backend environment.

The public installer remains:

```bash
curl -fsSL https://get.nethera.io/agent | sudo sh
```
