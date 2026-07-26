# Self-host n8n with a public URL

Example Nethera app for the [n8n recipe](https://nethera.io/docs/recipes/n8n).

## Secrets

Set these before deploying:

```bash
neth secrets set N8N_ENCRYPTION_KEY $(openssl rand -hex 32)
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
