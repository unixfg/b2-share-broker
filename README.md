# b2-share-broker

Installable web share app for publishing one file at a time to a public
Backblaze B2 bucket.

The browser authenticates with Keycloak, sends the selected file to the
same-origin upload API, and immediately receives an unlisted share URL. A
single internal processor stages the bytes, normalizes video to web-friendly
MP4, hashes the final bytes, uploads only the final object to B2, and marks the
share ready.

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

The cluster does not proxy downloaded file bytes.

### `GET /api/session`

Returns the current browser session state. Authenticated browser responses
include a CSRF token that must be sent in `X-CSRF-Token` on unsafe API requests.

### `POST /api/uploads`

Accepts either an authenticated browser session plus CSRF token or a Keycloak
OIDC bearer token with the configured share role.

Request is `multipart/form-data`:

- `file`: required, exactly one file

Response:

```json
{
  "shareUrl": "https://share.doesthings.online/s/0123abcd89ef4567-screenshot_1.png",
  "slug": "0123abcd89ef4567-screenshot_1.png",
  "jobId": "9f4e04ba-4a59-4d5b-aa08-74827eea7469",
  "status": "queued"
}
```

Generated aliases use a random 16-hex prefix plus a sanitized filename and the
final output extension. Video uploads get `.mp4` aliases because the processor
normalizes them before publication. Custom aliases are not supported.

### `GET /api/uploads/{jobId}`

Returns owner-only upload processing status. Completed jobs include the target
SHA-256 and B2 object key.

### `GET /api/shares`

Returns recent aliases owned by the current subject with filenames, sizes,
content types, status, redirect counts, share URLs, and native B2 URLs when the
share is ready.

### `DELETE /api/shares/{slug}`

Requires owner auth. Browser requests also require `X-CSRF-Token`.

Deletion keeps the alias row as soft-deleted metadata so redirect counts and
history survive. Staged files and queued jobs for that alias are removed or
canceled. The B2 object is hard-deleted only when no non-deleted aliases still
reference it.

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

## GitOps Deployment

The bees deployment lives in `github.com/unixfg/gitops` under
`apps/b2-share-broker`.

Before merging the GitOps PR, set the SOPS-managed B2 application key,
`OIDC_CLIENT_SECRET`, `SESSION_AUTH_KEY`, and Postgres credentials. User access
is controlled by Keycloak roles, not a ConfigMap subject list.

## Development

```bash
go test ./...
go build ./cmd/b2-share-broker
go build ./cmd/b2-share-processor
```

Native iOS/macOS share extensions are deferred. Apple users can use the browser
UI in v1.
