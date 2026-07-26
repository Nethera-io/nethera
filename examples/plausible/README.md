# Self-host Plausible Analytics with a public URL

Example Nethera app for the [plausible recipe](https://nethera.io/docs/recipes/plausible).

## Secrets

Set these before deploying:

```bash
neth secrets set SECRET_KEY_BASE $(openssl rand -hex 64)
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
