# Self-host Ollama with a public URL

Example Nethera app for the [ollama recipe](https://nethera.io/docs/recipes/ollama).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

This example exposes Ollama with `auth: token`. When `neth deploy` prompts, create an endpoint token and use it as the bearer token for API requests.

The `postDeploy` step pulls `llama3.2` into the `ollama` volume.
