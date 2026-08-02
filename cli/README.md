# Nethera CLI

Source for the `neth` command-line tool.

The CLI is used to log in, pair machines, initialise projects, manage secrets,
deploy `nethera.yml`, stream logs, inspect usage, and manage endpoint tokens.

## Development

Run tests:

```bash
go test ./...
```

Run against a local backend:

```bash
NETHERA_API_URL=http://127.0.0.1:8081 go run . login
```

Build locally:

```bash
go build -o neth .
```

## Release

Downloads are built and published by `nethera-ops`.

The default ops layout expects this repository next to the others:

```text
projects/
  nethera-cli/
  nethera-agent/
  nethera-ops/
```

Then:

```bash
cd ../nethera-ops
./deploy/release-deploy-downloads.sh staging v0.1.58
```

The public install script still installs the normal command:

```bash
curl -fsSL https://get.nethera.io/cli | sh
```
