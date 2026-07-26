# Self-host Paperless-ngx with a public URL

Example Nethera app for the [paperless-ngx recipe](https://nethera.io/docs/recipes/paperless-ngx).

## Secrets

Set these before deploying:

```bash
neth secrets set PAPERLESS_SECRET_KEY $(openssl rand -hex 32)
neth secrets set PAPERLESS_ADMIN_PASSWORD <admin-password>
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
