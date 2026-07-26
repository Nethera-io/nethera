# Self-host a GitLab Runner with Nethera

Example Nethera app for the [ci-runner recipe](https://nethera.io/docs/recipes/ci-runner).

## Secrets

Set these before deploying:

```bash
neth secrets set GITLAB_URL https://gitlab.com
neth secrets set GITLAB_RUNNER_TOKEN <runner-token>
```

## Deploy

From this directory:

```bash
neth init
neth deploy
```

Open the endpoint printed by `neth deploy`.
