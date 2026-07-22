# Development Guide

`b2-share-broker` is one Go module that builds three binaries and embeds its
browser assets into the broker package.

## Repository Layout

| Path | Purpose |
|---|---|
| `cmd/b2-share-broker` | Browser app, OIDC login, history, and public links |
| `cmd/b2-share-processor` | Upload API and queue worker |
| `cmd/b2-share-transcoder` | Worker-only compatibility entrypoint |
| `internal/broker/server.go` | HTTP API, upload staging, public shares, and deletion |
| `internal/broker/transcoder.go` | Queue, remux, transcode, enrichment, and upload pipeline |
| `internal/broker/metadata.go` | PostgreSQL models, queries, and migrations |
| `internal/broker/store.go` | Backblaze B2 S3-compatible object storage |
| `internal/broker/key.go` | Slugs, object keys, and extension normalization |
| `internal/broker/web` | Embedded PWA HTML, JavaScript, CSS, manifest, and icon |
| `chart` | OCI-published Helm chart |
| `docs` | User, API, architecture, deployment, and operations documentation |

## Local Stack

The Compose stack uses the published `:main` image rather than building local
source automatically. It provides Traefik, broker, processor, and PostgreSQL;
OIDC and B2 remain external.

```bash
cp .env.example .env
docker compose up
```

Open `http://localhost:8080`. See [Deployment](deployment.md#docker-compose)
for required settings and GPU limitations.

To test a local image through Compose, build and tag the image, then override
the service image through a Compose override file or temporary environment-
specific configuration.

## Build and Test

The supported workstation workflow runs Go in a container. The `:Z` volume
flag is required for Podman with SELinux:

```bash
docker run --rm \
  -v /home/ryan/Projects/b2-share-broker:/src:Z \
  -v b2-share-broker-gomodcache:/go/pkg/mod \
  -v b2-share-broker-gobuildcache:/root/.cache/go-build \
  -w /src golang:1.25-alpine \
  sh -c 'gofmt -l . && go build ./... && go vet ./... && go test ./...'
```

`gofmt -l .` must print nothing. Run formatting before the full check when
needed:

```bash
docker run --rm \
  -v /home/ryan/Projects/b2-share-broker:/src:Z \
  -w /src golang:1.25-alpine \
  gofmt -w .
```

The application image itself builds static Linux AMD64 binaries with Go 1.25
and runs them on `debian:bookworm-slim` with Jellyfin ffmpeg 7.

## Testing Style

Keep tests at the package boundary with the existing in-memory fakes:

- `memoryMetadata` for metadata operations;
- `fakeStore` for object storage;
- `fakeMediaRunner` for ffmpeg and ffprobe behavior.

Do not introduce SDK mocks when the local fake interfaces cover the behavior.
Handler tests belong in `server_test.go`; queue and media tests belong in
`transcoder_test.go`.

When changing media commands, test the exact argument sequence. ffmpeg
filtergraph quoting is parsed by ffmpeg, not a shell, and ffprobe output can
contain trailing CSV fields.

## Database Changes

Migrations run at every process startup and are serialized with an advisory
lock. Append rolling-deploy-safe, idempotent statements to the existing
migration sequence.

Use `IF NOT EXISTS` and `DROP CONSTRAINT IF EXISTS` where applicable. Add new
columns with defaults so old and new application versions can overlap. Do not
rewrite migrations that have already shipped.

Source upload hashes are intentionally not foreign keys to `objects`; original
uploads are temporary files, not stored object rows.

## Web Assets

The browser app uses embedded static HTML, CSS, JavaScript, a web manifest, and
a service worker under `internal/broker/web`. No separate frontend build step
exists.

Preserve the current behavior when changing the PWA:

- `/` is the landing page;
- `/share` is the authenticated upload/history shell;
- the service worker intercepts installed Web Share Target POSTs;
- shared files are temporarily handed off through IndexedDB;
- the service worker does not provide offline application caching.

Test both desktop and mobile layouts and the uninstalled `/share-target`
fallback.

## Helm Changes

For chart changes, run:

```bash
helm lint chart/
helm template b2-share-broker chart/ > /tmp/b2-share-broker-rendered.yaml
```

Increment `version` in `chart/Chart.yaml` whenever templates change. CI
publishes the packaged chart to `oci://ghcr.io/unixfg` on pushes to `main`.

## CI Artifacts

Pull requests run Go tests, build the broker, lint the chart, and render the
chart. Pushes to `main` also publish:

- the `ghcr.io/unixfg/b2-share-broker:main` image;
- a `:sha-<commit>` image tag;
- a chart artifact tagged with `chart/Chart.yaml`'s version.

The current image build targets Linux AMD64 only.

## Documentation Changes

Update the topic guide that owns the changed behavior:

- user workflow changes go in [User Guide](user-guide.md);
- endpoint changes go in [API Reference](api.md);
- processing and data-model changes go in [Architecture](architecture.md);
- environment changes go in [Configuration](configuration.md);
- installation changes go in [Deployment](deployment.md);
- procedures and failure modes go in [Operations](operations.md).

Keep the root README short and public-facing. Significant multi-change efforts
can add a dated record under `docs/worklogs`, but worklogs are historical and
must not replace current reference documentation.

Read [AGENTS.md](../AGENTS.md) before making repository changes for the full
engineering and deployment constraints.
