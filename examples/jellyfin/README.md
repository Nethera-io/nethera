# Access Jellyfin remotely with a public URL

Example Nethera app for the [jellyfin recipe](https://nethera.io/docs/recipes/jellyfin).

## Notes

Copy media to `/mnt/nethera/jellyfin/media` on the target machine before expecting Jellyfin to find it.

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
