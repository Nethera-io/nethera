# Self-host Vaultwarden with a public URL

Example Nethera app for the [vaultwarden recipe](https://nethera.io/docs/recipes/vaultwarden).

## Secrets

Set these before deploying:

```bash
neth secrets set ADMIN_TOKEN $(openssl rand -base64 48)
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
