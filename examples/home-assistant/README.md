# Self-host Home Assistant with a public URL

Example Nethera app for the [home-assistant recipe](https://nethera.io/docs/recipes/home-assistant).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`, then complete Home Assistant onboarding.

This example includes `configuration.yaml` so Home Assistant trusts Nethera's reverse proxy addresses. It also uses `preferLan: true`; use the LAN endpoint printed by `neth deploy` when you are on the same network.
