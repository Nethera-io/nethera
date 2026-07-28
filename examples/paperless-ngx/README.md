# Self-host Paperless-ngx with a public URL

Example Nethera app for the [paperless-ngx recipe](https://nethera.io/docs/recipes/paperless-ngx).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set PAPERLESS_SECRET_KEY $(openssl rand -hex 32)
neth secrets set PAPERLESS_DBPASS $(openssl rand -hex 24)
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first Paperless-ngx admin account.
