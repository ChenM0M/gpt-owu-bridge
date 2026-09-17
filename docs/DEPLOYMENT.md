# Container deployment

The image contains the local administrator CLI and an authenticated MCP server.
Without `GATE_PUBLIC_URL`, it exposes only loopback health endpoints. With a
public URL, it requires an initialized installation and a verified OWU account
before serving OAuth and MCP. A healthy container alone does not prove that
ChatGPT authorization and tool invocation have passed.

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

## Share URL input

`sync preview --url https://chatgpt.com/share/UUID -- [config flags]` fetches
anonymous HTML before running the same deterministic parser and preview.
Only canonical HTTPS share URLs are accepted. Redirects, cookies, non-HTML,
empty responses and responses over 8 MiB are rejected. `HTTPS_PROXY` may route
source requests through an administrator-managed proxy. Do not put proxy
credentials in command arguments, logs or public configuration. The OWU client
uses its own transport; source proxy configuration does not change its target.

The current OWU adapter requires a non-empty server `DEPLOYMENT_ID`. Stock
self-hosted OWU may return an empty value; this must be resolved before sync
initialization. Do not substitute a guessed deployment identity or silently
disable the identity check.

## Authenticated connector mode

Initialize once with `sync init`, using the same private volume and OWU token
file as the service. Set `GATE_PUBLIC_URL=https://YOUR_HOST/bridge`,
`GATE_OWU_BASE_URL`, and `GATE_OWU_TOKEN_FILE`, then recreate the container.
The data directory must have mode 0700 and belong to UID 65532. The image sets
these permissions for new volumes; check existing volumes during migration.

Forward `/bridge/` without stripping the prefix. Also forward exactly
`/.well-known/oauth-authorization-server/bridge` and
`/.well-known/oauth-protected-resource/bridge/mcp`. Preserve Authorization and
Cookie headers, disable response buffering, allow at least 90 seconds for
source fetching and writes, and never log token or authorization request bodies.
Use HTTPS externally. Keep the application's listen address on loopback.

The OAuth authorization-code and refresh flows use go-oauth2/oauth2 v4.6.0,
with mandatory S256 PKCE, exact registered redirects, fixed resource binding,
short-lived access tokens, rotating refresh tokens and revocation. Clients
register as public clients; only ChatGPT HTTPS callback URLs are accepted.
Authorization requires the deployment owner's existing OWU session cookie and
an explicit CSRF-protected consent form. This deployment assumes OWU and the
connector share a trusted HTTPS origin. It does not accept the OWU API Key as
an MCP bearer token. Credentials and tokens are stored only in the private
runtime volume; backups must be protected as secrets.

The MCP endpoint is `/bridge/mcp`. Tools are `preview_sync`, `apply_sync`, and
`sync_status`. Preview does not write OWU; apply requires confirmation of a
persisted, unexpired plan. All tools use the persisted installation owner, not
an owner or target chat ID supplied by the model. Automatic matching of changed
source identities remains disabled pending lifecycle validation. Unknown write
outcomes require reconciliation; the service never blindly retries them.

OAuth clients and tokens persist in `oauth.sqlite`. Preserve it together with
`gate.db` during backup/restore. Stop the service before copying both databases.
