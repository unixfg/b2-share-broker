# API Reference

The API uses JSON responses except for public HTML pages, redirects, and plain
HTTP `404` responses. API errors generally have this shape:

```json
{"error":"message"}
```

Replace `https://share.example.com` in examples with your deployment's
`PUBLIC_BASE_URL`.

## Authentication

Protected endpoints accept either a browser session or an OIDC bearer token.
The authenticated subject owns every share it creates.

### Bearer Tokens

Send an OIDC token with the configured issuer, audience, and at least one role
from `OIDC_REQUIRED_ROLES`:

```http
Authorization: Bearer eyJ...
```

Roles can come from Keycloak realm roles or client roles for
`OIDC_CLIENT_ID`. Bearer-token requests do not require CSRF protection.

### Browser Sessions and CSRF

Browser login creates a signed `b2_share_session` cookie. Unsafe API requests
made with that session must include the token returned by `GET /api/session`:

```http
X-CSRF-Token: token-from-api-session
```

`POST /api/uploads`, `PATCH /api/shares/{slug}`, and
`DELETE /api/shares/{slug}` require this header for session authentication.

## Endpoint Summary

| Method | Route | Authentication | Purpose |
|---|---|---|---|
| `GET`, `HEAD` | `/healthz` | None | Process health |
| `GET`, `HEAD` | `/api/session` | Optional | Browser session state |
| `POST` | `/api/uploads` | Session + CSRF or bearer | Queue one upload |
| `GET`, `HEAD` | `/api/uploads/{jobId}` | Owner | Read job status |
| `GET`, `HEAD` | `/api/shares` | Authenticated | List current shares |
| `PATCH` | `/api/shares/{slug}` | Owner + CSRF for session | Rename a share |
| `DELETE` | `/api/shares/{slug}` | Owner + CSRF for session | Delete a share |
| `GET`, `HEAD` | `/s/{slug}` | None | Open a public share |
| `GET`, `HEAD` | `/s/{slug}/media` | None | Open stored media |
| `GET`, `HEAD` | `/s/{slug}/thumbnail` | None | Open video thumbnail |
| `OPTIONS` | `/s/*` | None | Public-share CORS preflight |

## Health

### `GET /healthz`

Returns `200 OK` with `ok`. This is a process health check only; it does not
test PostgreSQL, B2, ffmpeg, staging storage, the GPU, or queue progress.

## Session

### `GET /api/session`

Returns `200 OK` whether or not the request is authenticated.

Unauthenticated response:

```json
{
  "authenticated": false
}
```

Authenticated browser response:

```json
{
  "authenticated": true,
  "user": {
    "sub": "248289761001",
    "email": "person@example.com",
    "preferred_username": "person",
    "roles": ["b2-share-user"]
  },
  "csrfToken": "signed-session-csrf-token"
}
```

Bearer-authenticated responses omit `csrfToken`.

## Uploads

### `POST /api/uploads`

Accepts `multipart/form-data` with:

| Field | Required | Description |
|---|---|---|
| `file` | Yes | Exactly one uploaded file |
| `name` | No | Desired public name; limited to 4096 bytes |

Example:

```bash
export TOKEN='eyJ...'

curl --fail-with-body \
  -X POST 'https://share.example.com/api/uploads' \
  -H "Authorization: Bearer ${TOKEN}" \
  -F 'file=@./clip.mov' \
  -F 'name=launch-demo.mov'
```

The response is `202 Accepted` because processing is asynchronous:

```json
{
  "shareUrl": "https://share.example.com/s/launch-demo.mp4",
  "slug": "launch-demo.mp4",
  "jobId": "9f4e04ba-4a59-4d5b-aa08-74827eea7469",
  "status": "queued"
}
```

The returned share URL exists immediately. A pending link serves a processing
page until the worker completes.

Naming behavior:

- Supplied names are sanitized and lowercased.
- Detected videos always receive a final `.mp4` extension.
- Other files retain a normalized detected extension or use `.bin`.
- Omitted names include a random 16-character hexadecimal suffix.
- Current, former, and deleted names cannot be reused.

Common errors:

| Status | Cause |
|---|---|
| `400` | Missing or invalid multipart upload, missing file, or multiple files |
| `401` | Missing or invalid authentication |
| `403` | Missing role or invalid browser CSRF token |
| `409` | Requested share name is already reserved |
| `413` | File exceeds `MAX_UPLOAD_BYTES` |

### `GET /api/uploads/{jobId}`

Returns owner-only processing status:

```bash
curl --fail-with-body \
  'https://share.example.com/api/uploads/9f4e04ba-4a59-4d5b-aa08-74827eea7469' \
  -H "Authorization: Bearer ${TOKEN}"
```

```json
{
  "jobId": "9f4e04ba-4a59-4d5b-aa08-74827eea7469",
  "status": "completed",
  "profile": "mp4-web",
  "slug": "launch-demo.mp4",
  "shareUrl": "https://share.example.com/s/launch-demo.mp4",
  "targetSha256": "9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827",
  "targetObjectKey": "9d/9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827.mp4"
}
```

Possible job states are `queued`, `running`, `completed`, `failed`, and
`canceled`. Failed or canceled jobs can include an `error` field. The profile
is `upload-finalize` for regular files and `mp4-web` for detected videos.

Jobs belonging to another subject return `404` rather than revealing their
existence. There is no job-cancel endpoint.

## Share Management

### `GET /api/shares`

Lists current, non-deleted shares owned by the authenticated subject, newest
updates first.

Query parameters:

| Parameter | Default | Description |
|---|---:|---|
| `q` | Empty | Case-insensitive slug, filename, status, or content-type search |
| `limit` | `50` | Result limit; maximum `100` |

```bash
curl --fail-with-body \
  'https://share.example.com/api/shares?q=mp4&limit=25' \
  -H "Authorization: Bearer ${TOKEN}"
```

```json
{
  "shares": [
    {
      "slug": "launch-demo.mp4",
      "sha256": "9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827",
      "objectKey": "9d/9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827.mp4",
      "owner": "248289761001",
      "displayFilename": "launch-demo.mp4",
      "visibility": "public",
      "status": "ready",
      "createdAt": "2026-07-22T10:20:30Z",
      "updatedAt": "2026-07-22T10:21:12Z",
      "redirectCount": 3,
      "size": 14502831,
      "contentType": "video/mp4",
      "width": 1920,
      "height": 1080,
      "thumbnailKey": "9d/9d2bb548dd140297cfdc2d1ab1d437b9e8604401279b6bcda1790700ee5f8827.jpg",
      "b2Url": "https://f000.backblazeb2.com/file/example/9d/example.mp4",
      "publicUrl": "https://share.example.com/s/launch-demo.mp4"
    }
  ]
}
```

Object, media, dimensions, error, and redirect-time fields can be absent or
zero while processing or when enrichment is unavailable. Retired names created
by renaming are not returned.

### `PATCH /api/shares/{slug}`

Renames a current share:

```bash
curl --fail-with-body \
  -X PATCH 'https://share.example.com/api/shares/launch-demo.mp4' \
  -H "Authorization: Bearer ${TOKEN}" \
  -H 'Content-Type: application/json' \
  --data '{"name":"conference-demo.mp4"}'
```

Success returns `200 OK` and the updated share object. The stored extension is
enforced regardless of the extension supplied in `name`.

The previous public URL returns a `308 Permanent Redirect` to the new URL and
remains globally reserved. Only the owner can rename the current name; retired
redirect aliases return `404`. A reserved destination returns `409`.

### `DELETE /api/shares/{slug}`

```bash
curl --fail-with-body \
  -X DELETE 'https://share.example.com/api/shares/conference-demo.mp4' \
  -H "Authorization: Bearer ${TOKEN}"
```

Success returns `204 No Content`.

The current alias and all former redirect aliases become unavailable. Queued
or running work is canceled. The stored object and thumbnail are permanently
deleted only when no other active current alias references them.

## Public Shares

### `GET /s/{slug}`

This endpoint is unauthenticated.

| Share condition | Response |
|---|---|
| Pending | `202` HTML processing page |
| Failed | `503` HTML unavailable page |
| Ready, regular client | `302` to the public B2 object |
| Ready, recognized crawler | `200` Open Graph page |
| Renamed | `308` to the current name |
| Missing, deleted, or non-public | `404` |

Regular ready requests count as opens. Crawler requests for the Open Graph page
do not.

### `GET /s/{slug}/media`

Returns `302` to the ready stored object and increments the open count. This
stable URL backs Open Graph video and image metadata.

### `GET /s/{slug}/thumbnail`

Returns `302` to an extracted JPEG without incrementing the open count. Returns
`404` for shares without a thumbnail.

Former names preserve `/media` and `/thumbnail` while redirecting to the
current name.

### Public CORS

`OPTIONS /s/*` returns `204`. If the request `Origin` exactly matches an entry
in `PUBLIC_SHARE_CORS_ALLOWED_ORIGINS`, responses allow `GET`, `HEAD`, and
`OPTIONS`, permit the `Range` header, and expose common object and redirect
headers.

The final media response comes from B2, so the bucket needs compatible CORS
rules as well.

## Authentication Routes

Browser clients normally let the web app manage these routes:

| Route | Purpose |
|---|---|
| `GET /auth/login?return_to=/share` | Start OIDC authorization code flow with PKCE |
| `GET /auth/callback` | Validate the provider response and create a session |
| `GET`, `POST /auth/logout` | Clear the browser session and redirect to `/` |

`return_to` accepts only safe same-origin relative paths. Invalid values and
authentication routes fall back to `/`.

## Web Routes

| Route | Purpose |
|---|---|
| `/` | Public landing page |
| `/share` | Browser upload and history application |
| `/docs` | Embedded short API examples |
| `/manifest.webmanifest` | PWA manifest |
| `/sw.js` | Web Share Target service worker |
| `/share-target` | Installed PWA share-target action |
