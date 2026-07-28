# Self-host Immich with a public URL

Example Nethera app for the [immich recipe](https://nethera.io/docs/recipes/immich).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first Immich admin account.

This example uses `preferLan: true`. In the Immich mobile app, use **Settings -> Networking** URL switching: set the Nethera HTTPS endpoint as the external URL and the LAN endpoint from `neth deploy` as the local URL.
