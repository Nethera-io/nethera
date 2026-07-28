# Access Jellyfin remotely with a public URL

Example Nethera app for the [jellyfin recipe](https://nethera.io/docs/recipes/jellyfin).

## Deploy

From this example directory, run:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`, then complete Jellyfin setup.

Copy media to the target machine before expecting Jellyfin to find it:

```bash
neth copy ./media <machine>:/mnt/nethera/jellyfin/media
```

This example uses `preferLan: true`; use the LAN endpoint printed by `neth deploy` when you are on the same network.
