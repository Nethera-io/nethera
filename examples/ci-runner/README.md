# Self-host a GitLab Runner with Nethera

Example Nethera app for the [ci-runner recipe](https://nethera.io/docs/recipes/ci-runner).

## Deploy

From this example directory, run:

```bash
neth init
neth secrets set GITLAB_URL "https://gitlab.com"
neth secrets set GITLAB_RUNNER_TOKEN "glrt-YOUR_TOKEN"
neth deploy
```

`GITLAB_RUNNER_TOKEN` is the runner authentication token from GitLab.

This example does not create a public endpoint. The `postDeploy` step registers the runner after deployment.
