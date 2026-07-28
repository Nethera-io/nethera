# Self-host Vaultwarden with a public URL

Example Nethera app for the [vaultwarden recipe](https://nethera.io/docs/recipes/vaultwarden).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`, then create the first Vaultwarden account immediately.

After the first account exists, set `SIGNUPS_ALLOWED: "false"` in `nethera.yml` and redeploy.
