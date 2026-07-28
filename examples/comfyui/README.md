# Self-host ComfyUI with a public URL

Example Nethera app for the [comfyui recipe](https://nethera.io/docs/recipes/comfyui).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.

ComfyUI can take a while on first startup while it downloads runtime dependencies. Watch progress with:

```bash
neth logs
```

This example uses `/mnt/nethera/comfyui` on the target machine for models and outputs. Copy models in with:

```bash
neth copy ./model.safetensors <machine>:/mnt/nethera/comfyui/models/checkpoints/
neth copy ./lora.safetensors <machine>:/mnt/nethera/comfyui/models/loras/
neth ls <machine>:/mnt/nethera/comfyui/models
```

Copy outputs back with:

```bash
neth copy <machine>:/mnt/nethera/comfyui/output ./comfyui-output
```
