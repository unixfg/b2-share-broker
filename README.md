# b2-share-broker

Installable web app for uploading personal share files directly to a public
Backblaze B2 bucket.

The broker never handles file bytes. The browser authenticates with Keycloak,
hashes the selected file with SHA-256, asks the broker for a presigned
S3-compatible PUT URL when the content is new, uploads one file directly to B2,
then receives a public unlisted URL to copy or share.

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

Unauthenticated public share link. The broker looks up `slug` in Postgres and
redirects public aliases to the stored native public B2 URL.

The cluster does not proxy downloaded file bytes.

### `GET /api/session`

Returns the current browser session state. Authenticated responses include a
CSRF token that must be sent in `X-CSRF-Token` on unsafe API requests.

### `POST /api/uploads`

Requires the authenticated browser session cookie and a matching
`X-CSRF-Token` header.

Request:

```json
{
  "filename": "Screenshot 1.png",
  "contentType": "image/png",
  "size": 12345,
  "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "alias": "optional-custom-name"
}
```

New-content response:

```json
{
  "uploadUrl": "https://...",
  "requiredHeaders": {
    "Content-Type": "image/png"
  },
  "objectKey": "s/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.png",
  "uploadToken": "...",
  "publicUrl": "https://share.doesthings.online/s/hmacalias.png",
  "b2Url": "https://<bucket>.s3.us-west-004.backblazeb2.com/s/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.png",
  "alreadyUploaded": false
}
```

The browser must upload the file bytes to `uploadUrl` with every returned
`requiredHeaders` value.

If the SHA-256 already exists in metadata, the response has
`alreadyUploaded: true`, omits `uploadUrl` and `uploadToken`, and records only
the new alias.

### `POST /api/uploads/complete`

Requires the same authenticated browser session that created the upload target.

Request:

```json
{
  "uploadToken": "..."
}
```

Response:

```json
{
  "objectKey": "s/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.png",
  "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "publicUrl": "https://share.doesthings.online/s/hmacalias.png",
  "b2Url": "https://<bucket>.s3.us-west-004.backblazeb2.com/s/0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.png",
  "verified": true,
  "size": 12345,
  "etag": "..."
}
```

The broker records object and alias metadata only after the B2 `HEAD` check
verifies the object.

### `GET /api/shares`

Requires an authenticated browser session. Returns recent aliases owned by the
current subject with filenames, sizes, content types, redirect counts, share
URLs, and native B2 URLs.

### `POST /api/shares/{slug}/processing-jobs`

Requires an authenticated browser session and `X-CSRF-Token`. Creates or returns
an in-flight owner-only processing job for a share alias.

Request:

```json
{
  "profile": "mp4-faststart-remux"
}
```

Response:

```json
{
  "jobId": "9f4e04ba-4a59-4d5b-aa08-74827eea7469",
  "status": "queued",
  "profile": "mp4-faststart-remux",
  "aliasSlug": "hmacalias.mp4",
  "sourceSha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

V1 enables only `mp4-faststart-remux`. The future
`mp4-h264-aac-discord` profile name is reserved but disabled.

### `GET /api/processing-jobs/{jobId}`

Requires an authenticated browser session. Returns the owner-only processing job
status. Completed jobs include the target SHA-256/object key.

## Broker Configuration

Required environment variables:

- `OIDC_ISSUER_URL`: Keycloak issuer, for bees
  `https://auth.doesthings.io/realms/doesthings.io`
- `OIDC_CLIENT_ID`: Keycloak web client ID, for bees `b2-share-web`
- `OIDC_CLIENT_SECRET`: Keycloak confidential client secret
- `OIDC_ALLOWED_SUBJECTS`: comma-separated allowed Keycloak subject IDs
- `B2_ENDPOINT`: S3-compatible endpoint, for bees
  `https://s3.us-west-004.backblazeb2.com`
- `B2_REGION`: defaults to `us-west-004`
- `B2_BUCKET`: existing public B2 bucket name
- `B2_PUBLIC_BASE_URL`: native public base URL for redirects to the bucket, for
  example
  `https://<bucket>.s3.us-west-004.backblazeb2.com`
- `PUBLIC_BASE_URL`: public base URL returned to users, for bees
  `https://share.doesthings.online`; defaults to `B2_PUBLIC_BASE_URL`
- `AWS_ACCESS_KEY_ID` or `ACCESS_KEY_ID`: B2 application key ID
- `AWS_SECRET_ACCESS_KEY` or `ACCESS_SECRET_KEY`: B2 application key
- `DATABASE_URL`: Postgres URL for share metadata
- `UPLOAD_TOKEN_KEY`: at least 32 bytes, or base64 encoding of at least 32 bytes
- `ALIAS_HMAC_KEY`: at least 32 bytes, or base64 encoding of at least 32 bytes
- `SESSION_AUTH_KEY`: at least 32 bytes, or base64 encoding of at least 32 bytes

Optional environment variables:

- `OBJECT_PREFIX`: defaults to `s`
- `MAX_UPLOAD_BYTES`: defaults to `536870912`
- `PRESIGN_TTL_SECONDS`: defaults to `900`
- `UPLOAD_TOKEN_TTL_SECONDS`: defaults to `3600`
- `SESSION_TTL_SECONDS`: defaults to `43200`
- `PORT` or `LISTEN_ADDR`: defaults to `:8080`
- `FFMPEG_PATH`: defaults to `ffmpeg`
- `TRANSCODER_WORK_DIR`: defaults to `/work`
- `TRANSCODER_POLL_SECONDS`: defaults to `5`

## Transcoder Worker

The container image includes two entrypoints:

- `/usr/local/bin/b2-share-broker`: browser app and API
- `/usr/local/bin/b2-share-transcoder`: internal queue worker

The worker polls Postgres for queued jobs, runs one job at a time, downloads the
source B2 object, executes:

```bash
ffmpeg -hide_banner -y -i input.mp4 -map 0 -c copy -movflags +faststart output.mp4
```

Then it hashes the output, uploads it as `s/<sha256>.mp4` if not already known,
records the derivative, and repoints the existing alias. Original uploaded B2
objects are retained.

## Keycloak Setup

Create a confidential client named `b2-share-web` in the `doesthings.online`
realm.

Minimum client settings:

- Standard flow enabled
- Client authentication enabled
- Valid redirect URIs include
  `https://share.doesthings.online/auth/callback`
- Web origins include `https://share.doesthings.online`
- No localhost redirect URI

After the first login, inspect the token subject and set
`OIDC_ALLOWED_SUBJECTS` in the GitOps ConfigMap.

## B2 CORS

The bucket must allow browser PUT uploads from
`https://share.doesthings.online`, including:

- Allowed origin: `https://share.doesthings.online`
- Allowed operations/methods: `s3_put`, `s3_head`, `s3_get`
- Allowed headers: `authorization`, `content-type`, `x-amz-*`
- Expose headers: `etag`

## GitOps Deployment

The bees deployment lives in `github.com/unixfg/gitops` under
`apps/b2-share-broker`.

Before merging the GitOps PR, replace the placeholder SOPS secret values with a
least-privilege B2 application key for the selected public bucket, set
`OIDC_CLIENT_SECRET`, set `SESSION_AUTH_KEY`, and set the real
`OIDC_ALLOWED_SUBJECTS`.

## Development

```bash
go test ./...
go build ./cmd/b2-share-broker
go build ./cmd/b2-share-transcoder
```

## Follow-up

Native iOS/macOS share extensions are deferred. Apple users can use the browser
UI in v1.
