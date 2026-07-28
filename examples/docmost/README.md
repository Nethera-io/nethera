# Self-host Docmost with a public URL

Example Nethera app for the [docmost recipe](https://nethera.io/docs/recipes/docmost).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set APP_SECRET $(openssl rand -hex 32)
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first Docmost admin account.
