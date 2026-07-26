# Self-host Home Assistant with a public URL

Example Nethera app for the [home-assistant recipe](https://nethera.io/docs/recipes/home-assistant).

## Notes

This example includes `configuration.yaml` so Home Assistant trusts Nethera's reverse proxy addresses.

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
