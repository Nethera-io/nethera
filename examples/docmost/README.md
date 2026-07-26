# Self-host Docmost with a public URL

Example Nethera app for the [docmost recipe](https://nethera.io/docs/recipes/docmost).

## Secrets

Set these before deploying:

```bash
neth secrets set APP_SECRET $(openssl rand -hex 32)
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
