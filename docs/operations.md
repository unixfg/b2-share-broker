# Operations Guide

This guide covers post-deployment checks, migrations, queue and media
troubleshooting, backups, and permanent B2 deletion.

## Health and Readiness

`GET /healthz` returns `200 OK` when the HTTP process can serve a request:

```bash
curl --fail https://share.example.com/healthz
```

It does not verify PostgreSQL, B2, OIDC, staging storage, ffmpeg, NVIDIA NVENC,
or worker progress. Treat it as a process probe, not a complete dependency
check.

After a rollout, also verify:

- browser login and `GET /api/session`;
- one non-video upload through completion;
- a B2 redirect from the resulting public URL;
- PostgreSQL connectivity and recent queue activity;
- staging PVC capacity and writability;
- NVENC processing with a video that actually requires transcoding;
- PostgreSQL backup status.

## Startup and Migrations

Every broker, processor, and compatibility worker process:

1. Loads and validates the complete environment configuration.
2. Initializes B2 and OIDC dependencies as required by its entrypoint.
3. Opens and pings PostgreSQL.
4. Obtains a PostgreSQL advisory lock.
5. Runs the embedded idempotent migration sequence.

Startup uses a 30-second setup context. The database user needs DDL privileges
because migrations create and alter tables, indexes, and constraints.

Migrations do not use a schema-version table. New migrations must remain
idempotent and safe when old and new application versions overlap during a
rolling deployment.

## Queue State

The processor claims the oldest `queued` job and handles one job at a time per
process. Its default idle poll interval is five seconds.

Inspect recent jobs:

```sql
SELECT id, alias_slug, profile, status, worker_id, created_at, started_at,
       completed_at, error
FROM processing_jobs
ORDER BY created_at DESC
LIMIT 25;
```

Inspect queue counts:

```sql
SELECT status, count(*)
FROM processing_jobs
GROUP BY status
ORDER BY status;
```

There is currently no lease, heartbeat, or automatic retry. A processor crash
after claiming a job can leave it in `running` indefinitely, with its staged
file still present. Confirm the processor and staging file state before making
any manual database correction.

## Staging Storage

Uploads stream to `STAGING_DIR` and remain there until completed, failed, or
deleted. Video work uses `TRANSCODER_WORK_DIR` for intermediate files.

Monitor:

- free space and inode use on the staging PVC;
- old `.upload`, `.upload.tmp`, and processing files;
- file ownership for UID/GID `65532`;
- jobs whose database state no longer matches a staged file.

There is no automatic orphan sweeper. Do not remove a staged file until you
have matched it to its processing job and confirmed the job no longer needs
it.

## Video Troubleshooting

### Verify the Encoder

Run inside the processor pod:

```bash
ffmpeg -hide_banner -h encoder=h264_nvenc
```

The output must describe the NVENC H.264 encoder and include the `-cq` option.
An Alpine ffmpeg build without NVENC will reject the application's transcode
arguments. The published image uses Jellyfin ffmpeg 7 for this reason.

### Verify NVIDIA Access

Check that the processor is scheduled with the expected runtime class and GPU
resource:

```bash
kubectl describe pod -n b2-share-broker PROCESSOR_POD
```

If compatible videos remux but HEVC or other inputs fail, confirm NVIDIA device
access before investigating the queue or B2.

### Enrichment Is Best Effort

Dimension probing and thumbnail extraction failures do not fail the job. Check
the stored metadata rather than relying only on transient logs:

```sql
SELECT sha256, object_key, width, height, thumbnail_key, status
FROM objects
ORDER BY created_at DESC
LIMIT 25;
```

Two ffmpeg details are intentional:

- ffprobe CSV parsing accepts trailing fields such as `1080,1920,`;
- the thumbnail filter keeps literal filtergraph quotes around
  `scale='min(1280,iw)':-2` even though `exec.Command` does not invoke a shell.

Changing either can silently remove dimensions or thumbnails while leaving
uploads otherwise successful.

## Deduplication Checks

Source derivative mappings should connect the original upload hash and profile
to a ready final object:

```sql
SELECT source_sha256, profile, target_sha256, created_at
FROM object_derivatives
ORDER BY created_at DESC
LIMIT 25;
```

The worker verifies a reusable target with B2 `HEAD`. Missing objects are
marked unavailable and uploaded again. Re-uploading a source whose ready
derivative already exists skips ffmpeg and does not backfill missing dimensions
or thumbnails.

## Public Links and Unfurls

Check a normal redirect:

```bash
curl -I https://share.example.com/s/example.mp4
```

Check the Open Graph page as Discord sees it:

```bash
curl \
  -A 'Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)' \
  https://share.example.com/s/example.mp4
```

The unfurl document should point media at `/s/{slug}/media` and, when present,
the JPEG at `/s/{slug}/thumbnail`.

Open counters include normal public redirects and `/media` fetches. They do not
represent unique viewers or video plays, and crawler retrieval of the unfurl
document itself is not counted.

## Permanent B2 Deletion

Backblaze B2 buckets are always versioned. A plain key-only S3 delete creates a
hide marker while older bytes remain stored and billed.

`B2Store.DeleteObject` lists versions for the exact key and removes every
version and hide marker by version ID. Verify deletion with a version-aware
listing:

```bash
b2 ls --recursive --long --versions b2://BUCKET_NAME
```

A plain `b2 ls` can make hidden objects look deleted.

Deletion is reference-aware. The API removes B2 bytes only when no active
current alias references the object. The main object and video thumbnail are
deleted together.

B2 deletion happens after the metadata transaction. A B2 failure is logged as
a warning and the API still returns `204`, so monitor delete warnings and verify
bucket versions when investigating storage that did not disappear.

## PostgreSQL Backups

The Helm chart's CloudNativePG configuration uses Barman object storage with
gzip compression, AES256 encryption, a 14-day retention policy, and a daily
scheduled backup by default.

The default destination and endpoint are empty placeholders. A ScheduledBackup
resource existing does not prove that backups are usable.

Verify:

- `cnpg.backup.destinationPath` and `cnpg.backup.endpointURL`;
- the backup credentials Secret;
- recent CNPG `Backup` resources and conditions;
- B2 backup objects and retention;
- a documented restoration test.

The chart currently configures backup creation, not bootstrap from backup.
Plan and test restoration separately.

## CORS

Application public-share CORS uses an exact allowlist and permits the `Range`
header. Because browsers follow redirects to B2, configure the bucket's CORS
rules for the same origin and required read headers.

Test both responses:

```bash
curl -I \
  -H 'Origin: https://consumer.example.com' \
  https://share.example.com/s/example.mp4
```

Then inspect the redirected B2 response with the same `Origin` header.

## Production Deployment Workflow

CI tests and builds pull requests. Pushes to `main` publish:

- `ghcr.io/unixfg/b2-share-broker:main`;
- `ghcr.io/unixfg/b2-share-broker:sha-<commit>`;
- `ghcr.io/unixfg/b2-share-broker:<chart-version>` as an OCI Helm chart.

Before updating production, verify that `:main` and `:sha-<commit>` resolve to
the expected image digest:

```bash
skopeo inspect docker://ghcr.io/unixfg/b2-share-broker:main
skopeo inspect docker://ghcr.io/unixfg/b2-share-broker:sha-COMMIT
```

Pin the verified `sha256:` value through `image.digest`. Increment
`chart/Chart.yaml` whenever chart templates change so CI publishes a new chart
artifact.

The reference production deployment is managed in
`github.com/unixfg/gitops` under `apps/b2-share-broker`. Application changes
should flow through Git and Argo CD rather than direct cluster mutation.
