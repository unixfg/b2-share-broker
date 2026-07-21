# b2-share-broker

Installable web share app for publishing one file at a time to a public
Backblaze B2 bucket.

The browser authenticates with Keycloak, sends the selected file to the
same-origin upload API, and immediately receives an unlisted share URL. A
single internal processor stages the bytes, normalizes video to web-friendly
MP4, hashes the final bytes, uploads only the final object to B2, and marks the
share ready.

## Runtime Shape

The app runs as two deployments plus a Postgres cluster:

- `b2-share-broker`: 2 HA replicas for UI, auth/session, history, and public
  redirects. Spread across nodes with `topologySpreadConstraints`; no GPU
  required.
- `b2-share-processor`: 1 replica using the `nvidia` RuntimeClass and one
  time-sliced `nvidia.com/gpu` share for video normalization. It exposes only
  the same internal HTTP API service used by the shared public route. Drop the
  GPU resource and `runtimeClassName` if you don't need video transcoding.
- `b2-share-broker-pg`: 3-instance CloudNativePG metadata database with B2
  backups.
- `b2-share-staging`: 20Gi RWO PVC for temporary upload staging.

`MAX_UPLOAD_BYTES` defaults to 2GiB for browser/PWA video uploads. Upload
files stream directly into staging rather than spilling through multipart
parser temp files.

## Web App

- `GET /` serves the fallback browser upload UI.
- `GET /share` serves the same UI and receives pending PWA share-target files.
- `GET /manifest.webmanifest` exposes the installable PWA manifest.
- `GET /sw.js` serves the service worker that handles Web Share Target POSTs.
- `POST /share-target` is reserved for the installed PWA share target and is
  not a server-side file upload fallback.

V1 accepts one file at a time. Public links are unlisted and readable by anyone
with the URL.

## API

### `GET /healthz`

Unauthenticated health check.

### `GET /s/{slug}`

Unauthenticated public share link.

- Pending shares return a minimal processing page.
- Failed shares return a minimal unavailable page.
- Ready shares increment redirect stats and redirect to the native public B2
  URL.
- Ready shares requested by known link unfurlers (Discord, Slack, iMessage,
  Mastodon, and similar crawlers) instead receive a minimal Open Graph page so
  chat apps embed the media inline: `og:video*` tags (with dimensions when
  known) and `og:image` point back at the share host — `/s/{slug}/media` and
  `/s/{slug}/thumbnail` — which redirect to the underlying B2 objects. Media
  endpoint fetches count as opens so embed traffic is visible in stats;
  thumbnail fetches do not. Crawler fetches of the unfurl page itself do not
  count as redirects.
- Renamed shares permanently redirect their former `/s/{slug}` URLs, including
  `/media` and `/thumbnail` variants, to the current name.

### `GET /s/{slug}/media`, `GET /s/{slug}/thumbnail`

Unauthenticated stable media URLs for a ready share. Both redirect to the B2
object (video/file bytes or extracted JPEG thumbnail); only `/media` increments
redirect stats. The thumbnail variant returns 404 when the object has no
extracted thumbnail.

The cluster does not proxy downloaded file bytes.

### `GET /api/session`

Returns the current browser session state. Authenticated browser responses
include a CSRF token that must be sent in `X-CSRF-Token` on unsafe API requests.

### `POST /api/uploads`

Accepts either an authenticated browser session plus CSRF token or a Keycloak
OIDC bearer token with the configured share role.

Request is `multipart/form-data`:

- `file`: required, exactly one file
- `name`: optional public share name; its extension is replaced with the final
  stored file extension

Response:

```json
{
  "shareUrl": "https://share.doesthings.online/s/screenshot_1-0123abcd89ef4567.png",
  "slug": "screenshot_1-0123abcd89ef4567.png",
  "jobId": "9f4e04ba-4a59-4d5b-aa08-74827eea7469",
  "status": "queued"
}
```

When `name` is omitted, the broker generates a random 16-hex suffix plus a
sanitized filename and final output extension. A supplied name is normalized to
a safe lowercase slug. Its extension is immutable: video uploads always use
`.mp4`, and other files retain their detected final extension. Names must be
globally unique, including retired names that continue redirecting old links.

### `GET /api/uploads/{jobId}`

Returns owner-only upload processing status. Completed jobs include the target
SHA-256 and B2 object key.

### `GET /api/shares`

Returns recent current share names owned by the current subject with filenames,
sizes, content types, status, redirect counts, share URLs, and native B2 URLs
when the share is ready. Retired names are not listed.

### `PATCH /api/shares/{slug}`

Requires owner auth. Browser requests also require `X-CSRF-Token`.

Renames a current share with a JSON body such as:

```json
{"name":"tiddies.mp4"}
```

The original file extension remains enforced regardless of the extension in
`name`. The response is the renamed share. The old public URL remains valid and
permanently redirects to the new URL; it cannot be reused by another share.

### `DELETE /api/shares/{slug}`

Requires owner auth. Browser requests also require `X-CSRF-Token`.

Deletion keeps the current alias and all of its retired redirect aliases as
soft-deleted metadata so redirect counts and history survive. Staged files and
queued jobs for that share are removed or canceled. The B2 object is
hard-deleted only when no non-deleted current aliases still reference it.
Because B2 buckets are always versioned and a key-only S3 delete only hides the
latest version, hard deletion removes every version and hide marker for the
object key by version ID.

## Object Storage

Final B2 object keys are content addressed and hash-sharded:

```text
20/2040110d78c97a48adc44851416f84662225d97af8798dbc2028359c843f08aa.mov
9d/9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827.mp4
```

Before skipping an upload for an existing metadata row, the processor verifies
the B2 object with `HEAD`. If the bucket object is missing, metadata is marked
unavailable and the final bytes are uploaded again.

## Processing

The image includes these entrypoints:

- `/usr/local/bin/b2-share-broker`: HA browser app, auth routes, history, and
  public redirects.
- `/usr/local/bin/b2-share-processor`: single-concurrency upload API plus queue
  worker with staging storage.
- `/usr/local/bin/b2-share-transcoder`: worker-only compatibility entrypoint.

The processor runs one job at a time. Non-video files are hashed and uploaded as
staged. Video files first try:

```bash
ffmpeg -hide_banner -y -i input -map 0 -c copy -movflags +faststart output.mp4
```

If remux fails or the remuxed MP4 is not H.264/AAC, the processor transcodes to
H.264/AAC MP4 with `h264_nvenc`. Original uploaded bytes are temporary staging
files and are not uploaded to the public bucket.

Processed videos are also probed for width and height, and a single-frame JPEG
thumbnail (seek 1s, falling back to 0s, capped at 1280px wide) is uploaded as a
sibling object keyed `<sha>.jpg` next to the `<sha>.mp4` video object. Both feed
the Open Graph unfurl page, and both are deleted together when the last
referencing share is removed.

Each upload's source bytes are hashed while streaming to staging and recorded on
the job. Completed jobs record a derivative from that source hash to the final
object per processing profile. A later upload of the same source bytes reuses
the existing ready object after the same `HEAD` verification and skips remux,
transcode, and upload entirely; aliases referencing one object share it until
the last non-deleted alias is removed.

## Configuration

Required environment variables:

- `OIDC_ISSUER_URL`: Keycloak issuer, for bees
  `https://auth.doesthings.online/realms/doesthings.online`
- `OIDC_CLIENT_ID`: Keycloak web client ID, for bees `b2-share-web`
- `OIDC_CLIENT_SECRET`: Keycloak confidential client secret
- `OIDC_AUDIENCE`: bearer-token audience, defaults to `OIDC_CLIENT_ID`
- `OIDC_REQUIRED_ROLES`: comma-separated Keycloak realm or client roles,
  defaults to `b2-share-user`
- `B2_ENDPOINT`: S3-compatible endpoint, for bees
  `https://s3.us-west-004.backblazeb2.com`
- `B2_REGION`: defaults to `us-west-004`
- `B2_BUCKET`: existing public B2 bucket name
- `B2_PUBLIC_BASE_URL`: native public base URL for redirects to the bucket
- `PUBLIC_BASE_URL`: public base URL returned to users, for bees
  `https://share.doesthings.online`
- `AWS_ACCESS_KEY_ID` or `ACCESS_KEY_ID`: B2 application key ID
- `AWS_SECRET_ACCESS_KEY` or `ACCESS_SECRET_KEY`: B2 application key
- `DATABASE_URL`: Postgres URL for share metadata
- `SESSION_AUTH_KEY`: at least 32 bytes, or base64 encoding of at least 32 bytes

Optional environment variables:

- `MAX_UPLOAD_BYTES`: defaults to `536870912`
- `SESSION_TTL_SECONDS`: defaults to `43200`
- `PUBLIC_SHARE_CORS_ALLOWED_ORIGINS`: comma-separated exact origins allowed
  to read public `/s/*` redirect responses with CORS, for example
  `https://discord.com`
- `PORT` or `LISTEN_ADDR`: defaults to `:8080`
- `FFMPEG_PATH`: defaults to `ffmpeg`
- `TRANSCODER_WORK_DIR`: defaults to `/work`
- `TRANSCODER_POLL_SECONDS`: defaults to `5`
- `STAGING_DIR`: defaults to `/staging`

## Keycloak Setup

Create a confidential client named `b2-share-web` in the `doesthings.online`
realm.

Minimum client settings:

- Standard flow enabled
- Client authentication enabled
- Valid redirect URIs include
  `https://share.doesthings.online/auth/callback`
- Valid post logout redirect URIs include `https://share.doesthings.online/`
- Web origins include `https://share.doesthings.online`
- No localhost redirect URI

Grant users a realm role or `b2-share-web` client role matching
`OIDC_REQUIRED_ROLES`, normally `b2-share-user`.

## Deployment

A Helm chart lives under `chart/` in this repository and is published as an
OCI artifact to `ghcr.io/unixfg/b2-share-broker`. It installs the two
Deployments (broker + processor), a Service for each, a ConfigMap, a PDB for
the broker, a staging PVC, and a CloudNativePG `Cluster` + `ScheduledBackup`
when `cnpg.enabled` is `true`.

```bash
helm install b2-share-broker oci://ghcr.io/unixfg/b2-share-broker \
  --version 0.1.0 \
  --namespace b2-share-broker \
  --create-namespace \
  -f values.yaml
```

See `chart/README.md` for the full values reference. The defaults in
`chart/values.yaml` are placeholders (`share.example.invalid`,
`keycloak.example.invalid`, empty storage classes) — override them in a
values file or with `--set`.

### Required secrets

Before installing, create a Secret (named `b2-share-broker-secrets` by
default, override via `secrets.existingSecret`) with these keys:

- `B2_ENDPOINT`, `B2_BUCKET`, `B2_PUBLIC_BASE_URL`
- `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`
- `OIDC_CLIENT_SECRET`
- `SESSION_AUTH_KEY` (at least 32 bytes, or base64 of at least 32 bytes)
- `DATABASE_URL`

The B2 application key must be able to read, write, `HEAD`, and delete
hash-sharded objects at the bucket root (there is no `s/` object prefix).

When `cnpg.enabled` is `true` (default), also create:
- `b2-share-broker-db` — CNPG bootstrap secret with `username` and `password`
- `b2-share-broker-b2-credentials` — CNPG backup credentials with
  `ACCESS_KEY_ID` and `ACCESS_SECRET_KEY`

User access is controlled by OIDC roles, not a ConfigMap subject list.

### Image

The chart references `ghcr.io/unixfg/b2-share-broker:main` by default. In
production pin a digest via `image.digest` (`sha256:<digest>`) and bump it
deliberately.

### Reference deployment

The bees cluster's full deployment (real URLs, bucket name, node affinity,
digest pin, SOPS secrets, Traefik routes, Gatus endpoint) lives in
`github.com/unixfg/gitops` under `apps/b2-share-broker`.

## Local Development

A `docker-compose.yaml` is included for local dev. It runs broker + processor
+ postgres behind a traefik reverse proxy that mirrors the production routing
(`/api/uploads` and `/api/shares` to the processor, everything else to the
broker).

```bash
cp .env.example .env
# edit .env to fill in OIDC issuer, B2 credentials, session key
docker compose up
```

The app is then at `http://localhost:8080`.

Without a GPU, remux of H.264/AAC video still works (stream copy), but NVENC
transcode won't. Non-video uploads work fully.

## Development

```bash
go test ./...
go build ./cmd/b2-share-broker
go build ./cmd/b2-share-processor
```

Native iOS/macOS share extensions are deferred. Apple users can use the browser
UI in v1.
