# Configuration Reference

All three binaries load environment variables through the same configuration
parser and validate the complete configuration at startup. As a result, even
the worker-only compatibility binary currently requires values such as OIDC and
session configuration that it does not use directly.

Strings are trimmed. Empty values use a documented fallback when one exists.

## Required Configuration

| Variable | Description |
|---|---|
| `OIDC_ISSUER_URL` | OIDC issuer URL used for discovery and token verification |
| `OIDC_CLIENT_ID` | Confidential web client ID |
| `OIDC_CLIENT_SECRET` | Client secret used by the browser authorization-code flow |
| `B2_ENDPOINT` | Backblaze S3-compatible endpoint URL |
| `B2_BUCKET` | Existing public bucket name |
| `B2_PUBLIC_BASE_URL` | Native public base URL used to construct object redirects |
| `AWS_ACCESS_KEY_ID` | B2 application key ID; `ACCESS_KEY_ID` is accepted as an alias |
| `AWS_SECRET_ACCESS_KEY` | B2 application key; `ACCESS_SECRET_KEY` is accepted as an alias |
| `DATABASE_URL` | PostgreSQL connection URL for metadata, jobs, and migrations |
| `SESSION_AUTH_KEY` | Session-signing key with at least 32 bytes of key material |

`OIDC_AUDIENCE`, `OIDC_REQUIRED_ROLES`, and `PUBLIC_BASE_URL` must also resolve
to non-empty values, but each has a fallback described below.

## Authentication and URLs

| Variable | Default | Description |
|---|---|---|
| `OIDC_ISSUER_URL` | Required | Provider issuer URL, such as `https://auth.example.com/realms/example` |
| `OIDC_CLIENT_ID` | Required | OIDC client ID |
| `OIDC_CLIENT_SECRET` | Required | Confidential client secret |
| `OIDC_AUDIENCE` | `OIDC_CLIENT_ID` | Required bearer-token audience |
| `OIDC_REQUIRED_ROLES` | `b2-share-user` | Comma-separated accepted realm or client roles |
| `PUBLIC_BASE_URL` | `B2_PUBLIC_BASE_URL` | External application origin used in share URLs and OIDC callbacks |
| `SESSION_AUTH_KEY` | Required | HMAC key for OAuth state and browser sessions |
| `SESSION_TTL_SECONDS` | `43200` | Browser session lifetime in seconds |

`PUBLIC_BASE_URL` should normally be the application origin, not the B2 object
origin. HTTPS controls whether browser session cookies receive the `Secure`
attribute.

The session key accepts:

- standard padded Base64 that decodes to at least 32 bytes;
- raw URL-safe Base64 that decodes to at least 32 bytes;
- a literal string containing at least 32 bytes.

Generate a suitable value with:

```bash
openssl rand -base64 32
```

## Backblaze B2

| Variable | Default | Description |
|---|---|---|
| `B2_ENDPOINT` | Required | S3-compatible endpoint, for example `https://s3.us-west-004.backblazeb2.com` |
| `B2_REGION` | `us-west-004` | AWS SDK signing region |
| `B2_BUCKET` | Required | Existing public bucket |
| `B2_PUBLIC_BASE_URL` | Required | Public object base URL without an object key |
| `AWS_ACCESS_KEY_ID` | Required | Preferred B2 application key ID variable |
| `AWS_SECRET_ACCESS_KEY` | Required | Preferred B2 application key variable |
| `ACCESS_KEY_ID` | None | Fallback alias for `AWS_ACCESS_KEY_ID` |
| `ACCESS_SECRET_KEY` | None | Fallback alias for `AWS_SECRET_ACCESS_KEY` |

The application credentials need to upload and inspect objects and to list and
delete every object version. See [B2 setup](deployment.md#backblaze-b2) for the
required behavior.

## Application and Processing

| Variable | Application default | Description |
|---|---:|---|
| `LISTEN_ADDR` | See below | Full HTTP listen address; takes precedence over `PORT` |
| `PORT` | `8080` | Port used as `:<PORT>` when `LISTEN_ADDR` is unset |
| `MAX_UPLOAD_BYTES` | `536870912` | Maximum file size in bytes; 512 MiB application default |
| `FFMPEG_PATH` | `ffmpeg` | Path or command used to invoke ffmpeg |
| `TRANSCODER_WORK_DIR` | `/work` | Temporary media-processing directory |
| `TRANSCODER_POLL_SECONDS` | `5` | Queue poll interval in seconds |
| `STAGING_DIR` | `/staging` | Directory for streamed upload staging |
| `PUBLIC_SHARE_CORS_ALLOWED_ORIGINS` | Empty | Comma-separated exact origins permitted on `/s/*` responses |

The default listen address is `:8080`. A non-empty `LISTEN_ADDR` overrides
`PORT` completely.

`MAX_UPLOAD_BYTES`, `SESSION_TTL_SECONDS`, and
`TRANSCODER_POLL_SECONDS` must be positive integers. Invalid non-integer text
silently falls back to the application default; parsed zero or negative values
fail validation.

`FFMPEG_PATH` configures ffmpeg only. The worker resolves `ffprobe` from
`PATH`.

## PostgreSQL

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | Required | PostgreSQL URL including scheme and host |

Each process opens the database, pings it, obtains an advisory migration lock,
and runs idempotent DDL at startup. The configured database user therefore
needs both normal data access and schema migration privileges.

## List Parsing

`OIDC_REQUIRED_ROLES` and `PUBLIC_SHARE_CORS_ALLOWED_ORIGINS` are
comma-separated lists. Values are trimmed, empty entries are removed, and
duplicates are removed while preserving order. Matching remains case-sensitive.

For CORS, every origin must:

- use `http` or `https`;
- include a host;
- contain no path, query, or fragment;
- exactly match the browser's `Origin` header.

Use `https://example.com`, not `https://example.com/`. Wildcards are not
supported. Because public media redirects to B2, configure compatible CORS on
the bucket separately.

## Validation Boundaries

Startup validation checks required values, URL syntax, positive numeric values,
session key length, CORS origins, and a non-empty staging directory. It does not
confirm:

- OIDC provider reachability;
- B2 bucket existence or permissions;
- PostgreSQL driver compatibility beyond URL syntax;
- staging or work-directory writability;
- ffmpeg, ffprobe, or NVIDIA device availability;
- listener address validity.

Use the checks in [Operations](operations.md) after deployment.

## Helm Mapping

The chart stores non-secret settings in a ConfigMap and reads secret values
from `secrets.existingSecret`.

| Helm value | Environment variable | Chart default |
|---|---|---|
| `config.port` | `PORT` | `8080` |
| `config.oidcIssuerUrl` | `OIDC_ISSUER_URL` | Empty |
| `config.oidcClientId` | `OIDC_CLIENT_ID` | `b2-share-web` |
| `config.oidcAudience` | `OIDC_AUDIENCE` | `b2-share-web` |
| `config.oidcRequiredRoles` | `OIDC_REQUIRED_ROLES` | `b2-share-user` |
| `config.publicBaseUrl` | `PUBLIC_BASE_URL` | Empty |
| `config.publicShareCorsAllowedOrigins` | `PUBLIC_SHARE_CORS_ALLOWED_ORIGINS` | Empty |
| `config.b2Region` | `B2_REGION` | `us-west-004` |
| `config.maxUploadBytes` | `MAX_UPLOAD_BYTES` | `2147483648` |
| `config.sessionTtlSeconds` | `SESSION_TTL_SECONDS` | `43200` |
| `config.ffmpegPath` | `FFMPEG_PATH` | `/usr/bin/ffmpeg` |
| `config.transcoderWorkDir` | `TRANSCODER_WORK_DIR` | `/work` |
| `config.transcoderPollSeconds` | `TRANSCODER_POLL_SECONDS` | `5` |
| `config.stagingDir` | `STAGING_DIR` | `/staging` |

The chart intentionally overrides the raw application upload default from 512
MiB to 2 GiB and the ffmpeg command from `ffmpeg` to `/usr/bin/ffmpeg`.

The existing Secret must use the canonical `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` keys because the chart explicitly references those
names. The application's shorter aliases are useful only outside the current
chart templates.

The Kubernetes ports and volume mounts currently assume port `8080`, staging at
`/staging`, and work files at `/work`. Keep those values unless the templates
are changed with them.

See the [chart reference](../chart/README.md) for deployment values and the
[deployment guide](deployment.md) for complete examples.
