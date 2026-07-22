# Deployment Guide

`b2-share-broker` needs an OIDC provider, PostgreSQL, a public Backblaze B2
bucket, and persistent upload staging. NVIDIA NVENC is required only for videos
that cannot be stream-copy remuxed as H.264/AAC MP4.

## Prerequisites

- A Linux AMD64 container host or Kubernetes cluster.
- An OIDC provider reachable by browsers and application pods.
- A confidential OIDC client and an authorized user role.
- An existing public Backblaze B2 bucket.
- B2 application credentials with object and version-management permissions.
- PostgreSQL reachable from every application process.
- Writable staging and work storage for the processor.
- An NVIDIA runtime and GPU resource for full video support.

## Keycloak

Keycloak is the reference OIDC provider, although the token verifier uses
standard OIDC discovery.

Create a confidential client, commonly named `b2-share-web`, with:

- Standard flow enabled.
- Client authentication enabled.
- A valid redirect URI of `https://share.example.com/auth/callback`.
- A valid post-logout redirect URI of `https://share.example.com/`.
- A web origin of `https://share.example.com`.

Create a realm role or client role, commonly `b2-share-user`, and grant it to
authorized users. Set `OIDC_REQUIRED_ROLES` to one or more accepted roles.

For local development, add the corresponding
`http://localhost:8080/auth/callback` redirect and
`http://localhost:8080` web origin. Do not retain localhost redirects on a
production client.

## Backblaze B2

Create a public bucket and note:

- the S3-compatible endpoint, such as
  `https://s3.us-west-004.backblazeb2.com`;
- the bucket name and region;
- the native public object base URL;
- an application key ID and application key.

The application key needs the equivalent of:

- `HeadObject` and `GetObject`;
- `PutObject`;
- `ListObjectVersions`;
- version-specific `DeleteObject`.

Version permissions are essential. Backblaze buckets are always versioned, and
a key-only S3 delete creates a hide marker instead of freeing stored bytes.

Objects are written at hash-sharded bucket-root keys such as
`9d/<sha256>.mp4`, not beneath the public `/s/` URL path.

If browser JavaScript needs cross-origin access to media after the redirect,
configure matching read-only CORS rules on both the application and B2 bucket.

## Docker Compose

The included stack runs Traefik, one broker, one processor, and PostgreSQL 16.
OIDC and B2 remain external services.

```bash
cp .env.example .env
```

Set at least these values in `.env`:

```dotenv
OIDC_ISSUER_URL=https://auth.example.com/realms/example
OIDC_CLIENT_ID=b2-share-web
OIDC_CLIENT_SECRET=replace-me
OIDC_AUDIENCE=b2-share-web
OIDC_REQUIRED_ROLES=b2-share-user

B2_ENDPOINT=https://s3.us-west-004.backblazeb2.com
B2_REGION=us-west-004
B2_BUCKET=replace-me
B2_PUBLIC_BASE_URL=https://f000.backblazeb2.com/file/replace-me
AWS_ACCESS_KEY_ID=replace-me
AWS_SECRET_ACCESS_KEY=replace-me

PUBLIC_BASE_URL=http://localhost:8080
SESSION_AUTH_KEY=replace-with-openssl-rand-base64-32
DATABASE_URL=postgres://b2_share_broker:devpassword@postgres:5432/b2_share_broker?sslmode=disable
```

Start the stack:

```bash
docker compose up
```

Open `http://localhost:8080`. Traefik sends upload and share-management APIs to
the processor and all remaining traffic to the broker.

The Compose file does not request a GPU. In that default mode, non-video files
work normally and compatible H.264/AAC videos can remux. Videos requiring
NVENC transcoding fail. Add host-specific NVIDIA container configuration when
full video support is required.

The Compose database credentials and mutable `:main` image are intended for
development, not production.

## Helm

The OCI chart deploys:

- two broker replicas by default;
- one processor using a `20Gi` RWO staging PVC;
- broker and processor ClusterIP Services;
- a broker PodDisruptionBudget and topology spread;
- an optional standard Kubernetes Ingress;
- an optional three-instance CloudNativePG cluster and scheduled backup.

### Namespace and Secrets

Create the namespace before creating Secrets:

```bash
kubectl create namespace b2-share-broker
```

Create the application Secret, or use your preferred secret-management system:

```bash
kubectl create secret generic b2-share-broker-secrets \
  --namespace b2-share-broker \
  --from-literal=B2_ENDPOINT='https://s3.us-west-004.backblazeb2.com' \
  --from-literal=B2_BUCKET='replace-me' \
  --from-literal=B2_PUBLIC_BASE_URL='https://f000.backblazeb2.com/file/replace-me' \
  --from-literal=AWS_ACCESS_KEY_ID='replace-me' \
  --from-literal=AWS_SECRET_ACCESS_KEY='replace-me' \
  --from-literal=OIDC_CLIENT_SECRET='replace-me' \
  --from-literal=SESSION_AUTH_KEY="$(openssl rand -base64 32)" \
  --from-literal=DATABASE_URL='postgres://b2_share_broker:replace-me@b2-share-broker-pg-rw:5432/b2_share_broker'
```

All broker and processor replicas must share the same session key.

### Values

Create a values file with environment-specific settings:

```yaml
image:
  digest: sha256:replace-me

namespace:
  create: false

config:
  oidcIssuerUrl: https://auth.example.com/realms/example
  oidcClientId: b2-share-web
  oidcAudience: b2-share-web
  oidcRequiredRoles: b2-share-user
  publicBaseUrl: https://share.example.com

processor:
  staging:
    storageClassName: replace-me

cnpg:
  storage:
    storageClassName: replace-me
  backup:
    destinationPath: s3://replace-me/postgres
    endpointURL: https://s3.us-west-004.backblazeb2.com

ingress:
  enabled: true
  className: replace-me
  host: share.example.com
  tls:
    enabled: true
    secretName: share-example-com-tls
```

Configure controller-specific request size and timeout annotations for uploads
as needed. The chart upload limit is 2 GiB and the processor allows two-hour
HTTP reads and writes, but an ingress controller can reject or time out the
request earlier.

Install chart version `0.1.2`:

```bash
helm install b2-share-broker oci://ghcr.io/unixfg/b2-share-broker \
  --version 0.1.2 \
  --namespace b2-share-broker \
  -f values.yaml
```

This example disables the chart-managed Namespace because the namespace was
created before its required Secrets. Alternatively, let the chart manage the
Namespace and provision Secrets through a controller that can create them
after installation.

See the [chart reference](../chart/README.md) for every value.

## CloudNativePG

With `cnpg.enabled: true`, install the CloudNativePG operator and create two
additional Secrets.

The bootstrap Secret defaults to `b2-share-broker-db` and needs `username` and
`password` keys:

```bash
kubectl create secret generic b2-share-broker-db \
  --namespace b2-share-broker \
  --from-literal=username='b2_share_broker' \
  --from-literal=password='replace-me'
```

The backup Secret defaults to `b2-share-broker-b2-credentials` and needs
`ACCESS_KEY_ID` and `ACCESS_SECRET_KEY`:

```bash
kubectl create secret generic b2-share-broker-b2-credentials \
  --namespace b2-share-broker \
  --from-literal=ACCESS_KEY_ID='replace-me' \
  --from-literal=ACCESS_SECRET_KEY='replace-me'
```

The chart does not derive `DATABASE_URL` from the bootstrap Secret. Put a valid
connection URL in the application Secret separately.

CNPG backups and the daily schedule are enabled by default, but
`cnpg.backup.destinationPath` and `cnpg.backup.endpointURL` are empty
placeholders. Set both before relying on backups. The default three-instance
cluster also requires three schedulable topology domains because pod
anti-affinity is required across hostnames.

## GPU Configuration

The default chart requests:

```yaml
processor:
  runtimeClassName: nvidia
  gpu:
    enabled: true
    count: 1
```

The cluster needs an NVIDIA runtime class, drivers, device plugin, and an
allocatable `nvidia.com/gpu` resource. Time-sliced GPU shares are supported if
configured by the cluster.

For a CPU-only deployment, disable both independent controls:

```yaml
processor:
  runtimeClassName: ""
  gpu:
    enabled: false
```

This does not enable software transcoding. Non-video files and remux-compatible
videos still work; videos that require transcoding fail.

## Routing

The built-in Ingress sends `/api/uploads` and `/api/shares` to the processor,
then sends `/` to the broker. Preserve the same path priorities when using
Traefik, Gateway API, or another external routing layer.

Routing uploads to the broker is unsupported in the chart topology because the
broker has short request timeouts, a read-only root filesystem, and no staging
volume.

## Production Checklist

- Use HTTPS for `PUBLIC_BASE_URL` and OIDC callbacks.
- Pin `image.digest` rather than relying on the mutable `main` tag.
- Restrict B2 credentials to the intended bucket.
- Confirm permanent version deletion permissions.
- Configure ingress body size and upload timeouts.
- Configure and test PostgreSQL backups and restoration.
- Choose storage classes for staging and CNPG.
- Verify NVENC inside the processor pod when full video support is expected.
- Monitor queue state, staging capacity, and B2 storage versions.

Continue with the [operations guide](operations.md) after installation.
