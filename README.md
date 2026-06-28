# b2-share-broker

Small authenticated broker for uploading personal share files directly to a
public Backblaze B2 bucket.

The broker never handles file bytes. Clients authenticate to the broker, ask for
a presigned S3-compatible PUT URL, upload directly to B2, then receive the
public B2 URL to copy or share.

## API

### `GET /healthz`

Unauthenticated health check.

### `POST /api/uploads`

Requires `Authorization: Bearer <oidc-access-token>`.

Request:

```json
{
  "filename": "Screenshot 1.png",
  "contentType": "image/png",
  "size": 12345
}
```

Response:

```json
{
  "uploadUrl": "https://...",
  "requiredHeaders": {
    "Content-Type": "image/png"
  },
  "objectKey": "share-broker/2026/06/28/01J.../Screenshot_1.png",
  "uploadToken": "...",
  "publicUrl": "https://bucket.s3.us-west-004.backblazeb2.com/share-broker/..."
}
```

The client must upload the file bytes to `uploadUrl` with every returned
`requiredHeaders` value.

### `POST /api/uploads/complete`

Requires the same authenticated principal that created the upload target.

Request:

```json
{
  "uploadToken": "..."
}
```

Response:

```json
{
  "objectKey": "share-broker/2026/06/28/01J.../Screenshot_1.png",
  "publicUrl": "https://bucket.s3.us-west-004.backblazeb2.com/share-broker/...",
  "verified": true,
  "size": 12345,
  "etag": "..."
}
```

If the B2 `HEAD` check is temporarily unavailable, the broker still returns the
URL with `verified: false`.

## Broker Configuration

Required environment variables:

- `OIDC_ISSUER_URL`: Keycloak issuer, for bees
  `https://auth.doesthings.io/realms/doesthings.io`
- `OIDC_AUDIENCE`: expected token audience/client ID, normally
  `b2-share-broker`
- `OIDC_ALLOWED_SUBJECTS`: comma-separated allowed Keycloak subject IDs
- `B2_ENDPOINT`: S3-compatible endpoint, for bees
  `https://s3.us-west-004.backblazeb2.com`
- `B2_REGION`: defaults to `us-west-004`
- `B2_BUCKET`: existing public B2 bucket name
- `B2_PUBLIC_BASE_URL`: native public base URL for the bucket, for example
  `https://<bucket>.s3.us-west-004.backblazeb2.com`
- `AWS_ACCESS_KEY_ID` or `ACCESS_KEY_ID`: B2 application key ID
- `AWS_SECRET_ACCESS_KEY` or `ACCESS_SECRET_KEY`: B2 application key
- `UPLOAD_TOKEN_KEY`: at least 32 bytes, or base64 encoding of at least 32 bytes

Optional environment variables:

- `OBJECT_PREFIX`: defaults to `share-broker`
- `MAX_UPLOAD_BYTES`: defaults to `536870912`
- `PRESIGN_TTL_SECONDS`: defaults to `900`
- `UPLOAD_TOKEN_TTL_SECONDS`: defaults to `3600`
- `PORT` or `LISTEN_ADDR`: defaults to `:8080`

## CLI

Install/build:

```bash
go install github.com/unixfg/b2-share-broker/cmd/b2-share@latest
```

Upload:

```bash
b2-share upload ./file.png
```

The CLI discovers the OIDC issuer, starts a local callback listener, performs a
PKCE browser login, stores tokens in the OS keychain when available, falls back
to a `0600` file in the user config directory, uploads directly to B2, prints
the public URL, and copies it to the clipboard when possible.

Environment overrides:

- `B2_SHARE_BROKER_URL`
- `B2_SHARE_OIDC_ISSUER`
- `B2_SHARE_OIDC_CLIENT_ID`

## Keycloak Setup

Create a public client named `b2-share-broker` in the
`doesthings.io` realm.

Minimum client settings:

- Standard flow enabled
- PKCE required with S256
- Client authentication disabled
- Valid redirect URIs include `http://127.0.0.1:*/*`
- Token audience includes `b2-share-broker`
- Offline access allowed if refresh tokens are desired

After the first login, inspect the token subject and set
`OIDC_ALLOWED_SUBJECTS` in the GitOps ConfigMap.

## GitOps Deployment

The bees deployment lives in `github.com/unixfg/gitops` under
`apps/b2-share-broker`.

Before merging the GitOps PR, replace the placeholder SOPS secret values with a
least-privilege B2 application key for the selected public bucket and set the
real `OIDC_ALLOWED_SUBJECTS`.

## Development

```bash
go test ./...
go build ./cmd/b2-share-broker
go build ./cmd/b2-share
```

## Follow-up

Add a separate PeerTube-oriented upload profile later. Do not reuse the generic
share bucket/prefix for PeerTube publishing without an explicit policy decision.

