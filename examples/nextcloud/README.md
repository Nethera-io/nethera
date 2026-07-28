# Access Nextcloud remotely with a public URL

Example Nethera app for the [nextcloud recipe](https://nethera.io/docs/recipes/nextcloud).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set MYSQL_PASSWORD
neth secrets set MYSQL_ROOT_PASSWORD
neth deploy
```

Open the endpoint printed by `neth deploy`, then complete the Nextcloud first-run setup.

This example uses `preferLan: true`. For heavy sync, configure desktop and mobile clients with the LAN endpoint printed by `neth deploy` when they mostly stay on the same network.
