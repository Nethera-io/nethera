# Self-host Open WebUI with Ollama and a public URL

Example Nethera app for the [open-webui recipe](https://nethera.io/docs/recipes/open-webui).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first Open WebUI account. That account becomes the admin.

Ollama is available to Open WebUI at `http://ollama:11434`; it is not exposed as a public endpoint.
