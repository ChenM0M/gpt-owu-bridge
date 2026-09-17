# Container deployment

The current image contains the M2 local administrator CLI and a loopback-only
health process. It does **not** yet expose MCP or OAuth. A healthy container is
not a successful ChatGPT integration.

## Build and publish

The `Verify and publish container` GitHub Actions workflow runs formatting,
module verification, race tests, vet, and the independent synthetic CLI test.
It then builds a Linux amd64 image and runs Compose health, rejected business
route, and restart checks before publishing to
`ghcr.io/chenm0m/gpt-owu-bridge:sha-COMMIT`.

GHCR package visibility is separate from repository visibility. Set the package
public for anonymous pulls, or authenticate the server with a read-only package
credential. Never store registry credentials in Compose or the repository.

## Linux server

Set `BRIDGE_IMAGE` in a private `.env` next to `compose.yaml` to the published
`ghcr.io/chenm0m/gpt-owu-bridge@sha256:DIGEST`. Then run:

```sh
docker compose pull
docker compose up -d --wait --wait-timeout 90
curl --fail http://127.0.0.1:18089/health/ready
docker compose restart
docker compose up -d --wait --wait-timeout 90
```

The service uses Linux host networking because the current application enforces
loopback binding. It listens only on `127.0.0.1:18089`; it does not publish a LAN
or public business endpoint. No change to an existing OWU container is required.
The container runs as UID/GID 65532, with a read-only root filesystem, a dedicated
named data volume, resource limits, and bounded logs. Keep old image digests to
roll back; change the image reference and run Compose again. Do not remove the
data volume. Stop all users of the data directory before taking an offline backup.

## Remaining integration gates

- Dedicated outbound connectivity capable of anonymously fetching a user-supplied
  ChatGPT share URL; a homepage HTTP response alone is insufficient.
- An OWU API credential for the intended account, stored outside the repository.
- Authenticated MCP tools, a mature OAuth provider, owner mapping, and a reachable
  HTTPS endpoint; domain selection may be deferred during container validation.
- Real ChatGPT authorization and tool invocation, followed by OWU page comparison.

Personal research notes, raw samples, proxy profiles and deployment credentials
are not part of the public source distribution.
