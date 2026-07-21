# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

`b2-share-broker` is an installable web share app that publishes one file at a
time to a public Backblaze B2 bucket behind stable permalinks
(`https://share.doesthings.online/s/{slug}`). Uploads are queued and processed
asynchronously; videos are normalized to web-friendly H.264/AAC MP4. Only
processed output is uploaded to the public bucket — original uploads stay as
temporary staging files.

Three binaries are built from one Go module and shipped in one image
(`ghcr.io/unixfg/b2-share-broker`):

- `cmd/b2-share-broker`: HA browser app (auth, history, public redirects/unfurl).
- `cmd/b2-share-processor`: single-concurrency upload API plus queue worker
  (pinned to the `fileserver` node with an NVIDIA GPU in production).
- `cmd/b2-share-transcoder`: worker-only compatibility entrypoint.

## Layout

- `internal/broker/server.go`: HTTP handlers, upload staging, public share +
  Open Graph unfurl rendering, share deletion.
- `internal/broker/transcoder.go`: queue worker, remux/transcode/thumbnail
  runner, derivative dedup, object upload.
- `internal/broker/metadata.go`: Postgres metadata store, migrations, alias /
  object / job / derivative models.
- `internal/broker/store.go`: B2 (S3-compatible) object store.
- `internal/broker/key.go`: slugs, content-addressed object keys, filename
  sanitizing. Note: `normalizeExtension` allows only ONE dot — multi-dot
  extensions like `.thumb.jpg` get inner dots stripped.
- `internal/broker/web/`: embedded PWA assets.
- `Dockerfile`: single image for all binaries; runtime is
  `debian:bookworm-slim` + `jellyfin-ffmpeg7` (NOT Alpine ffmpeg — see
  "Hard-won facts" below).
- `chart/`: Helm chart published as an OCI artifact to
  `ghcr.io/unixfg/b2-share-broker`. Deployment manifests for the broker,
  processor, CNPG cluster, PDBs, staging PVC, and optional Ingress.
- `docker-compose.yaml`: local dev stack (broker + processor + postgres
  behind traefik). Mirrors production routing.

## Build and test

There is no Go toolchain on the workstation. Run everything in the container:

```bash
docker run --rm \
  -v /home/ryan/Projects/b2-share-broker:/src:Z \
  -v b2-share-broker-gomodcache:/go/pkg/mod \
  -v b2-share-broker-gobuildcache:/root/.cache/go-build \
  -w /src golang:1.25-alpine \
  sh -c 'gofmt -l . && go build ./... && go vet ./... && go test ./...'
```

Notes:

- The `:Z` volume flag is required (podman + SELinux); without it the mount is
  empty and you get "directory prefix . does not contain main module".
- `gofmt -l .` must print nothing; run `gofmt -w .` first if it does.
- Keep the existing test style: in-memory fakes (`memoryMetadata`,
  `fakeStore`, `fakeMediaRunner`) in `server_test.go` / `transcoder_test.go`,
  no SDK mocks.

## CI/CD and deployment

1. PR into `main` (squash merge). CI runs tests, `helm lint chart/`, and
   `helm template chart/`; image and chart builds only run on `main` pushes
   and publish:
   - `ghcr.io/unixfg/b2-share-broker:main` and `:sha-<commit>` (OCI image)
   - `ghcr.io/unixfg/b2-share-broker:<chart-version>` (OCI Helm chart artifact)
2. Deployment lives in `github.com/unixfg/gitops` under `apps/b2-share-broker`
   as an Argo CD Application sourcing the published Helm chart. Bees-specific
   values (real URLs, node affinity, image digest, storage classes) are
   inline in `apps/b2-share-broker/helm/bees/application.yaml`. Bump
   `targetRevision` to the new chart version and `image.digest` to the new
   image digest in one gitops PR and merge; Argo CD syncs normally. Never
   mutate the cluster directly for app changes.
3. Verify the digest before opening the gitops PR:
   `skopeo inspect docker://ghcr.io/unixfg/b2-share-broker:main` must equal
   `:sha-<merge commit>`. Bump `version` in `chart/Chart.yaml` when chart
   templates change so the chart artifact republishes.

## Database conventions

- Migrations run at every pod startup (`runMigrations`, advisory-locked,
  idempotent). Append new `ALTER TABLE ... IF NOT EXISTS` /
  `DROP CONSTRAINT IF EXISTS` statements; never rewrite shipped ones.
- Add columns with defaults so rolling deploys are safe in either order.
- `objects` rows are content-addressed by SHA-256 of the stored bytes and are
  ref-counted through aliases. Thumbnails are keyed `<sha>.jpg` next to their
  `<sha>.mp4` and are owned 1:1 by the video object (deleted together).

## Hard-won operational facts

These cost real debugging time — do not relearn them the hard way:

- **B2 buckets are always versioned.** An S3 `DeleteObject` naming only the
  key creates a *hide marker*; the bytes stay stored and billed. Permanent
  deletion requires deleting every version and hide marker by `VersionId` —
  `B2Store.DeleteObject` does exactly that. Check leftovers with
  `b2 ls --recursive --versions b2://<bucket>`, not a plain `ls` (hide markers
  make the bucket look empty).
- **ffmpeg filtergraph quoting is not shell quoting.** `exec.Command` passes
  args literally, so filter expressions with commas must keep their
  filtergraph-level single quotes: `-vf scale='min(1280,iw)':-2`. Unquoted,
  ffmpeg parses `min(1280` as a filter name and fails with `Filter not found`.
- **ffprobe CSV output can have trailing fields** (observed `1080,1920,` from
  an NVENC-processed iPhone MOV). Parse the first two fields; never require
  exactly two (`parseVideoDimensions`).
- **NVENC needs jellyfin-ffmpeg.** Alpine's ffmpeg has no nvenc build flags,
  so `-c:v h264_nvenc -cq …` dies at argument parsing with
  `Unrecognized option 'cq'`. The image installs `jellyfin-ffmpeg7` and
  symlinks `/usr/bin/ffmpeg`/`ffprobe` to it. The processor pod needs
  `runtimeClassName: nvidia` and `nvidia.com/gpu: 1`.
- **Derivative dedup is source-keyed.** `processing_jobs.source_object_sha256`
  records the SHA-256 of the original upload (hashed while streaming to
  staging). `object_derivatives` maps `(source sha, profile)` → stored object;
  the worker short-circuits duplicates before any ffmpeg run. The
  `source_object_sha256` foreign keys were dropped deliberately: upload
  sources are ephemeral and never rows in `objects`.
- **Enrichment is warn-and-continue.** Dimension probing and thumbnail
  extraction failures must never fail a job; the unfurl page degrades
  gracefully (no `og:image`/dimension tags). Warn logs vanish when pods roll,
  so verify enrichment in the DB (`objects.width/height/thumbnail_key`), not
  just by absence of errors.
- **Unfurl counting semantics.** `/s/{slug}` answers known crawler user agents
  with an OG page (not counted). `/s/{slug}/media` 302s to B2 and increments
  the open count (this is how embed fetches become visible);
  `/s/{slug}/thumbnail` 302s without counting (would double-count unfurls).
  Counts are "link clicks + embed proxy fetches", never literal plays —
  Discord serves repeats from its own CDN.

## Live verification recipes

```bash
# DB state (CNPG primary)
kubectl exec -n b2-share-broker b2-share-broker-pg-1 -- \
  psql -U postgres -d b2_share_broker -c '<sql>'

# Bucket contents incl. hide markers / old versions
b2 ls --recursive --long --versions b2://shared-eqLTh8Kgwm5BVQ8RgZw2i6TWuUBnLynb

# Unfurl page as Discord sees it
curl -A "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)" \
  https://share.doesthings.online/s/<slug>

# ffmpeg/ffprobe behavior against a real object, in the processor pod
kubectl exec -n b2-share-broker <processor-pod> -- sh -c '...'
```

## Documentation expectations

- Update `README.md` whenever behavior, endpoints, or data model change.
- Update this `AGENTS.md` when workflows, pipelines, or operational knowledge
  change.
- Significant multi-PR efforts get a work log under `docs/` (see
  `docs/WORKLOG-2026-07-21-MEDIA-PIPELINE.md`).
