# Self-host NocoDB with a public URL

Example Nethera app for the [nocodb recipe](https://nethera.io/docs/recipes/nocodb).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set NC_AUTH_JWT_SECRET $(openssl rand -hex 32)
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first NocoDB admin account.
