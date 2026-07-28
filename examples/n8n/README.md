# Self-host n8n with a public URL

Example Nethera app for the [n8n recipe](https://nethera.io/docs/recipes/n8n).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set N8N_ENCRYPTION_KEY $(openssl rand -hex 32)
neth deploy
```

Open the endpoint printed by `neth deploy`, then complete n8n owner setup.
