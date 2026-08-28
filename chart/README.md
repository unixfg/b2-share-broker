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
- A NetworkPolicy-capable CNI when `networkPolicy.enabled` is `true`.

## Install

Create the required Secrets before installing, then provide an environment-
specific values file. If you create the namespace manually for those Secrets,
set `namespace.create: false` in that file.

```bash
helm install b2-share-broker oci://ghcr.io/unixfg/b2-share-broker \
  --version 0.1.6 \
  --namespace b2-share-broker \
  -f values.yaml
```

The defaults contain empty public URL, OIDC issuer, backup destination, backup
endpoint, and storage-class values. They are not a ready-to-run production
configuration.

The broker and processor set the runtime-default seccomp profile at both pod
and container scope. Their containers also run as UID/GID 65532 with privilege
escalation disabled, a read-only root filesystem, and all Linux capabilities
dropped.

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

## Network Policies

`networkPolicy.enabled` is `false` by default so upgrading the chart does not
change network reachability. When enabled, the chart renders ingress-only
policies with these boundaries:

| Target | Allowed ingress |
|---|---|
| Same-release API pods | Trusted Traefik and Gatus pods on TCP 8080 |
| Same-release processor pods | Trusted Traefik pods on TCP 8080 |
| `b2-share-broker-pg` CNPG instances | Same-release API and processor pods on TCP 5432; same-cluster CNPG workloads on TCP 5432 and instance-manager failsafe TCP 8000; trusted CNPG operator pods on TCP 8000 and 5432; trusted Prometheus pods on TCP 9187 |

The processor and database policies render only when their corresponding
components are enabled. These policies do not select egress traffic.

Every external source combines a namespace selector with a pod selector. The
defaults match the reference deployment's exact Traefik, Gatus, CloudNativePG
operator, and Prometheus release labels. Override the full LabelSelector maps
under `networkPolicy.trustedSources` if your namespaces or release labels are
different, and confirm those labels before enabling the policies.

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
| `networkPolicy.enabled` | bool | `false` | Create ingress-only NetworkPolicies |
| `networkPolicy.trustedSources.traefik.namespaceSelector` | object | See `values.yaml` | Trusted Traefik namespace LabelSelector |
| `networkPolicy.trustedSources.traefik.podSelector` | object | See `values.yaml` | Trusted Traefik pod LabelSelector |
| `networkPolicy.trustedSources.gatus.namespaceSelector` | object | See `values.yaml` | Trusted Gatus namespace LabelSelector |
| `networkPolicy.trustedSources.gatus.podSelector` | object | See `values.yaml` | Trusted Gatus pod LabelSelector |
| `networkPolicy.trustedSources.cnpgOperator.namespaceSelector` | object | See `values.yaml` | Trusted CNPG operator namespace LabelSelector |
| `networkPolicy.trustedSources.cnpgOperator.podSelector` | object | See `values.yaml` | Trusted CNPG operator pod LabelSelector |
| `networkPolicy.trustedSources.prometheus.namespaceSelector` | object | See `values.yaml` | Trusted Prometheus namespace LabelSelector |
| `networkPolicy.trustedSources.prometheus.podSelector` | object | See `values.yaml` | Trusted Prometheus pod LabelSelector |
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
- Review trusted source labels before enabling NetworkPolicies.
- Verify B2 versions after testing deletion.
- Validate NVENC from inside the processor pod.
