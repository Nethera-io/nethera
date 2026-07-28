# Self-host vLLM with a public HTTPS endpoint

Example Nethera app for the [vllm recipe](https://nethera.io/docs/recipes/vllm).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set HF_TOKEN
neth deploy
```

This example exposes the OpenAI-compatible API with `auth: token`. When `neth deploy` prompts, create an endpoint token and use it as the bearer token for API requests.

This example expects a GPU-enabled machine. If the model is public, you can remove `HF_TOKEN` from `nethera.yml` and skip the secret.
