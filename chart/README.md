# b2-share-broker Helm Chart

This chart deploys the browser-facing broker, upload processor, staging
storage, and an optional CloudNativePG cluster for
[`b2-share-broker`](../README.md).

See the main [deployment guide](../docs/deployment.md) for OIDC, Backblaze B2,
GPU, ingress, and production setup. See
[configuration](../docs/configuration.md) for application environment-variable
behavior.

## Prerequisites

- Kubernetes with a default or explicitly configured storage class.
- An existing public Backblaze B2 bucket and application key.
- An external OIDC provider and confidential client.
- CloudNativePG installed when `cnpg.enabled` is `true`.
- An NVIDIA runtime and GPU resource when processor GPU support is enabled.

## Install

Create the required Secrets before installing, then provide an environment-
specific values file. If you create the namespace manually for those Secrets,
set `namespace.create: false` in that file.

```bash
helm install b2-share-broker oci://ghcr.io/unixfg/b2-share-broker \
  --version 0.1.2 \
  --namespace b2-share-broker \
  -f values.yaml
```

The defaults contain empty public URL, OIDC issuer, backup destination, backup
endpoint, and storage-class values. They are not a ready-to-run production
configuration.

## Required Secrets

### Application

Create `secrets.existingSecret`, `b2-share-broker-secrets` by default, with:

| Key | Purpose |
|---|---|
| `B2_ENDPOINT` | Backblaze S3-compatible endpoint |
| `B2_BUCKET` | Existing public bucket |
| `B2_PUBLIC_BASE_URL` | Native public base URL for object redirects |
| `AWS_ACCESS_KEY_ID` | B2 application key ID |
| `AWS_SECRET_ACCESS_KEY` | B2 application key |
| `OIDC_CLIENT_SECRET` | Confidential OIDC client secret |
| `SESSION_AUTH_KEY` | At least 32 bytes, or Base64 decoding to at least 32 bytes |
| `DATABASE_URL` | PostgreSQL connection URL |

The B2 key must support object `HEAD` and upload plus object-version listing and
version-specific deletion. All broker and processor replicas must share the
same session key.

### CloudNativePG

When `cnpg.enabled` is `true`, also create:

- `cnpg.credentials.existingSecret`, default `b2-share-broker-db`, with
  `username` and `password`.
- `cnpg.backupCredentials.existingSecret`, default
  `b2-share-broker-b2-credentials`, with `ACCESS_KEY_ID` and
  `ACCESS_SECRET_KEY`.

The chart does not generate the application's `DATABASE_URL` from the CNPG
bootstrap Secret.

## Routing

When enabled, the built-in Ingress routes:

| Path | Service |
|---|---|
| `/api/uploads`, `/api/uploads/*` | Processor |
| `/api/shares`, `/api/shares/*` | Processor |
| Everything else | Broker |

Set controller-specific request-body and timeout annotations for large uploads.
The processor allows long-running upload requests, but ingress defaults can be
much lower.

## GPU

The processor requests `runtimeClassName: nvidia` and one `nvidia.com/gpu`
resource by default. Disable both controls for a CPU-only deployment:

```yaml
processor:
  runtimeClassName: ""
  gpu:
    enabled: false
```

CPU-only mode does not provide software transcoding. Non-video uploads and
H.264/AAC videos that can be remuxed still work.

## Values

| Key | Type | Default | Description |
|---|---|---|---|
| `commonLabels` | object | `{}` | Additional common resource labels |
| `image.repository` | string | `ghcr.io/unixfg/b2-share-broker` | OCI image repository |
| `image.tag` | string | `main` | Image tag |
| `image.digest` | string | `""` | Optional `sha256:` production pin |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `namespace.create` | bool | `true` | Render the Namespace resource |
| `namespace.labels` | object | `{}` | Additional Namespace labels |
| `broker.replicas` | int | `2` | Broker replica count |
| `broker.revisionHistoryLimit` | int | `2` | Broker ReplicaSet history |
| `broker.resources` | object | See `values.yaml` | Broker requests and limits |
| `broker.nodeSelector` | object | `{}` | Broker node selector |
| `broker.affinity` | object | `{}` | Broker affinity |
| `broker.tolerations` | list | `[]` | Broker tolerations |
| `broker.topologySpreadConstraints.enabled` | bool | `true` | Spread brokers across nodes |
| `broker.topologySpreadConstraints.maxSkew` | int | `1` | Maximum topology skew |
| `broker.topologySpreadConstraints.topologyKey` | string | `kubernetes.io/hostname` | Spread topology key |
| `broker.topologySpreadConstraints.whenUnsatisfiable` | string | `DoNotSchedule` | Spread failure behavior |
| `processor.enabled` | bool | `true` | Deploy processor, Service, and API ingress routes |
| `processor.replicas` | int | `1` | Processor replica count |
| `processor.revisionHistoryLimit` | int | `2` | Processor ReplicaSet history |
| `processor.runtimeClassName` | string | `nvidia` | RuntimeClass; set empty to disable |
| `processor.gpu.enabled` | bool | `true` | Request `nvidia.com/gpu` |
| `processor.gpu.count` | int | `1` | Requested GPU shares |
| `processor.resources` | object | See `values.yaml` | Processor requests and limits |
| `processor.nodeSelector` | object | `{}` | Processor node selector |
| `processor.affinity` | object | `{}` | Processor affinity |
| `processor.tolerations` | list | `[]` | Processor tolerations |
| `processor.staging.size` | string | `20Gi` | Staging PVC size |
| `processor.staging.storageClassName` | string | `""` | Staging storage class |
| `processor.staging.accessMode` | string | `ReadWriteOnce` | Staging PVC access mode |
| `config.port` | string | `8080` | Application port; templates currently assume 8080 |
| `config.oidcIssuerUrl` | string | `""` | OIDC issuer URL |
| `config.oidcClientId` | string | `b2-share-web` | OIDC client ID |
| `config.oidcAudience` | string | `b2-share-web` | Bearer-token audience |
| `config.oidcRequiredRoles` | string | `b2-share-user` | Comma-separated accepted roles |
| `config.publicBaseUrl` | string | `""` | Public application URL |
| `config.publicShareCorsAllowedOrigins` | string | `""` | Comma-separated exact CORS origins |
| `config.b2Region` | string | `us-west-004` | B2 signing region |
| `config.maxUploadBytes` | string | `2147483648` | Maximum upload size in bytes |
| `config.sessionTtlSeconds` | string | `43200` | Session lifetime in seconds |
| `config.ffmpegPath` | string | `/usr/bin/ffmpeg` | ffmpeg executable |
| `config.transcoderWorkDir` | string | `/work` | Processor work directory |
| `config.transcoderPollSeconds` | string | `5` | Queue poll interval |
| `config.stagingDir` | string | `/staging` | Upload staging directory |
| `secrets.existingSecret` | string | `b2-share-broker-secrets` | Application Secret name |
| `pdb.enabled` | bool | `true` | Create broker PDB |
| `pdb.minAvailable` | int | `1` | Minimum available brokers |
| `cnpg.enabled` | bool | `true` | Deploy CloudNativePG resources |
| `cnpg.instances` | int | `3` | PostgreSQL instance count |
| `cnpg.description` | string | See `values.yaml` | Cluster description |
| `cnpg.storage.size` | string | `10Gi` | Storage per PostgreSQL instance |
| `cnpg.storage.storageClassName` | string | `""` | PostgreSQL storage class |
| `cnpg.resources` | object | See `values.yaml` | PostgreSQL requests and limits |
| `cnpg.backup.retentionPolicy` | string | `14d` | Barman retention policy |
| `cnpg.backup.destinationPath` | string | `""` | Backup object-store destination |
| `cnpg.backup.endpointURL` | string | `""` | Backup S3-compatible endpoint |
| `cnpg.credentials.existingSecret` | string | `b2-share-broker-db` | Bootstrap credentials Secret |
| `cnpg.backupCredentials.existingSecret` | string | `b2-share-broker-b2-credentials` | Backup credentials Secret |
| `cnpg.drainPdb.enabled` | bool | `true` | Create CNPG drain PDB |
| `cnpg.drainPdb.minAvailable` | int | `2` | Minimum available PostgreSQL pods |
| `cnpg.scheduledBackup.enabled` | bool | `true` | Create daily ScheduledBackup |
| `cnpg.scheduledBackup.schedule` | string | `0 0 9 * * *` | Six-field CNPG schedule |
| `ingress.enabled` | bool | `false` | Create standard Ingress |
| `ingress.className` | string | `""` | Ingress class |
| `ingress.annotations` | object | `{}` | Ingress annotations |
| `ingress.host` | string | `""` | Public host |
| `ingress.tls.enabled` | bool | `false` | Enable TLS block |
| `ingress.tls.secretName` | string | `""` | TLS Secret name |

Keep `config.port` at `8080`, `config.stagingDir` at `/staging`, and
`config.transcoderWorkDir` at `/work` unless the corresponding Service, probe,
and volume-mount templates are changed.

## Production Notes

- Pin `image.digest`; the default `main` tag is mutable.
- Set storage classes explicitly when the cluster has no suitable defaults.
- Configure both CNPG backup destination fields before relying on backups.
- Keep one processor replica with the default RWO staging topology.
- Verify B2 versions after testing deletion.
- Validate NVENC from inside the processor pod.
