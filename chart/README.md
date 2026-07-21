# b2-share-broker Helm Chart

Installable web share app for publishing one file at a time to a public
Backblaze B2 bucket behind stable permalinks.

## Install

```bash
helm install b2-share-broker oci://ghcr.io/unixfg/b2-share-broker \
  --version 0.1.0 \
  --namespace b2-share-broker \
  --create-namespace \
  -f values.yaml
```

## Required Secrets

Before installing, create a Secret named `b2-share-broker-secrets` (or whatever
you set `secrets.existingSecret` to) with these keys:

- `B2_ENDPOINT` - S3-compatible B2 endpoint URL
- `B2_BUCKET` - existing public B2 bucket name
- `B2_PUBLIC_BASE_URL` - native public base URL for redirects to the bucket
- `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` - B2 application key credentials
- `OIDC_CLIENT_SECRET` - Keycloak confidential client secret
- `SESSION_AUTH_KEY` - at least 32 bytes (or base64 of at least 32 bytes)
- `DATABASE_URL` - Postgres URL for share metadata

If `cnpg.enabled` is `true` (default), also create:
- `b2-share-broker-db` - CNPG bootstrap secret with `username` and `password` keys
- `b2-share-broker-b2-credentials` - CNPG backup credentials with `ACCESS_KEY_ID`
  and `ACCESS_SECRET_KEY` keys

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `image.repository` | string | `ghcr.io/unixfg/b2-share-broker` | OCI image repository |
| `image.tag` | string | `main` | OCI image tag |
| `image.digest` | string | `""` | Pin a sha256 digest for production rollouts |
| `image.pullPolicy` | string | `IfNotPresent` | Kubernetes image pull policy |
| `namespace.create` | bool | `true` | Create the Namespace resource |
| `broker.replicas` | int | `2` | HA broker Deployment replica count |
| `broker.resources` | object | see values | Broker container resources |
| `broker.nodeSelector` | object | `{}` | Broker node selector |
| `broker.affinity` | object | `{}` | Broker affinity rules |
| `broker.tolerations` | list | `[]` | Broker tolerations |
| `broker.topologySpreadConstraints.enabled` | bool | `true` | Spread broker pods across nodes |
| `processor.enabled` | bool | `true` | Deploy the processor |
| `processor.replicas` | int | `1` | Processor Deployment replica count |
| `processor.runtimeClassName` | string | `nvidia` | Runtime class for NVENC; set to `""` to disable |
| `processor.gpu.enabled` | bool | `true` | Request `nvidia.com/gpu` resource |
| `processor.gpu.count` | int | `1` | GPU shares requested |
| `processor.resources` | object | see values | Processor container resources |
| `processor.nodeSelector` | object | `{}` | Processor node selector |
| `processor.affinity` | object | `{}` | Processor affinity rules |
| `processor.tolerations` | list | `[]` | Processor tolerations |
| `processor.staging.size` | string | `20Gi` | Staging PVC size |
| `processor.staging.storageClassName` | string | `""` | Staging PVC storage class |
| `config.*` | object | see values | ConfigMap data (OIDC, B2, app settings) |
| `secrets.existingSecret` | string | `b2-share-broker-secrets` | Name of existing Secret with env keys |
| `pdb.enabled` | bool | `true` | Create a PDB for broker pods |
| `pdb.minAvailable` | int | `1` | Minimum available broker pods |
| `cnpg.enabled` | bool | `true` | Deploy a CloudNativePG Cluster |
| `cnpg.instances` | int | `3` | CNPG Postgres instances |
| `cnpg.storage.size` | string | `10Gi` | CNPG storage size |
| `cnpg.storage.storageClassName` | string | `""` | CNPG storage class |
| `cnpg.backup.destinationPath` | string | `""` | CNPG Barman backup destination |
| `cnpg.backup.endpointURL` | string | `""` | CNPG Barman endpoint URL |
| `cnpg.credentials.existingSecret` | string | `b2-share-broker-db` | CNPG bootstrap secret name |
| `cnpg.backupCredentials.existingSecret` | string | `b2-share-broker-b2-credentials` | CNPG backup secret name |
| `cnpg.drainPdb.enabled` | bool | `true` | Create a CNPG drain PDB |
| `cnpg.scheduledBackup.enabled` | bool | `true` | Create a daily ScheduledBackup |
| `cnpg.scheduledBackup.schedule` | string | `0 0 9 * * *` | Cron schedule for backups |
| `ingress.enabled` | bool | `false` | Create a standard Kubernetes Ingress |
| `ingress.className` | string | `""` | Ingress class name |
| `ingress.host` | string | `""` | Public hostname |
| `ingress.tls.enabled` | bool | `false` | Enable TLS on the Ingress |
| `ingress.tls.secretName` | string | `""` | TLS secret name |
