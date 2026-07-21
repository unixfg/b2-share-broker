# Work log: 2026-07-21 — share media pipeline hardening and embeds

A record of the six-change effort that fixed video transcoding, made deletion
actually remove bytes, added source-hash dedup, and made share links unfurl
into inline media players in chat apps.

All app changes are in this repository (PRs #17–#22). Deployment changes are
in `github.com/unixfg/gitops` (PRs #771–#776), each bumping the pinned
`ghcr.io/unixfg/b2-share-broker:main@sha256:<digest>` in
`apps/b2-share-broker/base/{deployment,processor-deployment}.yaml` and synced
by Argo CD.

## 1. NVENC transcoding (#17, gitops #771)

**Problem.** Uploads of non-H.264/AAC video (e.g. HEVC from iMessage) failed:
`ffmpeg transcode failed: exit status 8: Unrecognized option 'cq'`.

**Root cause.** The transcode path hardcodes `-c:v h264_nvenc -preset p4 -cq 23`.
`-cq` is an NVENC-only option, and the Alpine ffmpeg in the image was built
without nvenc, so argument parsing failed. The error proves ffmpeg was present
— a missing binary would fail at exec.

**Fix.** Runtime base moved to `debian:bookworm-slim` with `jellyfin-ffmpeg7`
from the Jellyfin apt repo (`--enable-nvenc --enable-cuvid --enable-cuda
--enable-ffnvcodec`), symlinked to `/usr/bin/ffmpeg`/`ffprobe`.

**Verification.** In the GPU-pinned processor pod: `ffmpeg -h
encoder=h264_nvenc` lists `-cq`, and a smoke test with the app's exact
transcode flags completed against the NVIDIA GPU.

## 2. Source-hash recording and derivative dedup (#18, gitops #772)

**Problem.** Only the *processed* output was hashed. `object_derivatives` and
`processing_jobs.source_object_sha256` existed but were never populated, so
uploading the same video twice re-paid the full NVENC transcode cost (live
evidence: the same video transcoded twice, jobs `e55e1e16` and `14bda55c`,
converging on one object only after the second transcode).

**Fix.**

- `stageUpload` hashes source bytes while streaming to staging (no extra read
  pass); the hash is stored on the job as `source_object_sha256`.
- Before any ffmpeg work, the worker checks
  `GetDerivedObject(source, profile)`; on a hit it verifies the target with the
  existing `readyObject` `HEAD` check and completes the job pointing the alias
  at the existing object — no remux, transcode, or re-upload.
- Completion records the derivative, seeding future dedup.
- Migrations drop the two `source_object_sha256` foreign keys
  (`processing_jobs`, `object_derivatives`): upload sources are ephemeral
  staging files, never rows in `objects`.

**Note.** Non-video (`upload-finalize`) uploads deduped already, since final
bytes equal source bytes. Re-uploading a source whose derivative target was
deleted re-processes normally (`GetDerivedObject` filters to ready objects).

## 3. Version-aware B2 deletion (#19, gitops #773)

**Problem.** Deleting a share in the web UI did not remove bytes from the
bucket.

**Root cause.** B2 buckets are always versioned; an S3 `DeleteObject` naming
only the key creates a *hide marker* (confirmed live: hide marker timestamped
exactly at the API call, full upload version still stored). Earlier "delete
works" observations were wrong — the bucket had been cleaned manually. A plain
`b2 ls` hides this; `--versions` is required.

**Fix.** `B2Store.DeleteObject` paginates `ListObjectVersions` for the exact
key and permanently deletes every version and hide marker by `VersionId`.

**Verification (empirical, with the app's own credentials).** Key-only delete
reproduced the hide-marker behavior; version-ID deletion removed everything
(the app's B2 key already had the needed capability); the real leftover was
cleaned the same way, leaving the bucket at zero versions.

## 4. Open Graph unfurl pages, dimensions, thumbnails (#20, gitops #774)

**Problem.** Share links 302-redirect to B2, so chat unfurlers (Discord,
Slack, iMessage, …) had nothing embeddable — only the raw B2 link inline-played.

**Fix.**

- `/s/{slug}` sniffs the User-Agent against a known-unfurler list
  (`isUnfurlAgent`). Crawlers get a minimal OG page: `og:title`/`og:url`/
  `og:site_name`/`theme-color`, `og:video*` (+`og:video:width/height` when
  known) for videos, `og:image` for thumbnails or `image/*` shares. Everyone
  else keeps the instant 302. Crawler fetches don't count as redirects.
- Processor probes processed videos for width/height (ffprobe) and extracts a
  one-frame JPEG thumbnail (seek 1s, 0s fallback, ≤1280px wide), uploaded as
  sibling object `<sha>.jpg`. New `objects` columns `width`, `height`,
  `thumbnail_key` (defaults, rolling-deploy safe).
- Deleting the last share referencing an object now also deletes its
  thumbnail. Enrichment failures are warn-and-continue.

**Rejected alternatives.** Byte proxying through the broker (bandwidth through
small nodes, Range support); branded browser landing page (breaks
direct-media consumers); vanity slugs (deliberately removed earlier).

## 5. Counting embed media fetches (#21, gitops #775)

**Problem.** Embedded plays in Discord were invisible to stats: Discord's
media proxy fetched bytes straight from the `og:video` B2 URL.

**Fix.** OG tags now point at broker media endpoints that 302 to B2:

- `/s/{slug}/media` — increments the existing open count (backs `og:video`,
  and `og:image` for `image/*` shares).
- `/s/{slug}/thumbnail` — does not count (fetched on every unfurl).

Semantics: "opens" = link clicks + embed proxy fetches (first unfurl + cache
misses). Never literal per-viewer plays — Discord serves repeats from its CDN.

## 6. Enrichment bug fixes (#22, gitops #776)

**Problem.** Post-deploy verification showed processed videos got neither
dimensions nor thumbnails; warn logs were lost in a pod rollout. Both failures
reproduced live in the processor pod against real object bytes.

**Root causes and fixes.**

1. ffprobe on a real NVENC-processed MOV prints `1080,1920,` (trailing empty
   field); a strict two-field parser rejected it. `parseVideoDimensions` now
   tolerates extra fields (unit-tested with the production case).
2. `-vf scale=min(1280,iw):-2` unquoted made ffmpeg parse `min(1280` as a
   filter → `Filter not found`. The single quotes are ffmpeg *filtergraph*
   quotes, not shell quotes — restored as literal arg characters:
   `scale='min(1280,iw)':-2`. Verified verbatim in the pod (valid 112 KB
   frame extracted).

## Deploy timeline (2026-07-21, UTC)

| GitOps PR | App PR | Image digest (prefix) | Synced revision |
|-----------|--------|-----------------------|-----------------|
| #771 | #17 | `3bf7b08b` | `054b67f9` |
| #772 | #18 | `bf3b881e` | `a7d83e76` |
| #773 | #19 | `390adbf4` | `2425f053` |
| #774 | #20 | `717fe5b6` | `f5cb6704` |
| #775 | #21 | `236c3b05` | `e3e341bc` |
| #776 | #22 | `70aa769d` | `dd6386c4` |

Each digest was verified with `skopeo inspect` to match both `:main` and
`:sha-<merge commit>` before opening the gitops PR.

## Known leftovers and follow-ups

- Two video objects processed before #22 (`fb953018…`, `a539f597…`) have no
  dimensions/thumbnail. Re-uploading the same files hits derivative dedup and
  will not backfill. Optional one-off backfill: probe dims and extract a
  thumbnail from the stored MP4 in the processor pod, PUT the `<sha>.jpg`
  object, and UPDATE the `objects` rows. Requires explicit authorization (live
  B2/DB mutation).
- Optional defense-in-depth: a B2 lifecycle rule with
  `daysFromHidingToDeleting` on the bucket to auto-purge future hide markers
  (set via Backblaze console or native API; not Git-managed).
- If Discord's media proxy ever refuses the 302 from `/s/{slug}/media`,
  embeds regress to plain links; the revert is repointing `og:video`/
  `og:image` at direct B2 URLs.
- `go mod why`: `github.com/jackc/*` is pgx (the Postgres driver, used via
  `pgx/v5/stdlib`); `golang.org/x/text` is an indirect, go.mod-level
  requirement of pgx — downloaded but not compiled into the binary.
