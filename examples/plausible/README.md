# Self-host Plausible Analytics with a public URL

Example Nethera app for the [plausible recipe](https://nethera.io/docs/recipes/plausible).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set SECRET_KEY_BASE $(openssl rand -hex 64)
neth deploy
```

Open the endpoint printed by `neth deploy`, then complete Plausible setup.
