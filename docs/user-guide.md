# User Guide

`b2-share-broker` publishes one file at a time behind a stable, unlisted URL.
Authentication is required to create and manage shares; opening a public share
does not require authentication.

## Browser Workflow

1. Open `/share` on your deployment.
2. Sign in through the configured OIDC provider.
3. Choose or drop one file.
4. Accept the generated share name or enter a custom name.
5. Select **Upload**.
6. Copy the public URL while processing continues in the background.

The page polls the processing job and updates its status. History shows your
current shares, including status, content type, size, open count, and update
time.

The root `/` route is a public landing page. The upload application lives at
`/share`.

## Installable PWA

The web app includes a manifest and service worker, so supported browsers can
install it. On platforms that implement Web Share Target, installed B2 Share
appears as a destination in the system share sheet.

The share target accepts exactly one file. Its service worker stores the file
temporarily in browser IndexedDB, opens `/share`, and restores the pending file
after authentication. It does not cache the application for offline use.

If an uninstalled browser posts directly to `/share-target`, the server rejects
the upload and directs the user to install the web app first.

## Share Names

When no name is supplied, B2 Share combines a sanitized filename with a random
16-character hexadecimal suffix. Custom names are normalized to safe lowercase
slugs.

The processed file type controls the extension:

- Recognized videos always use `.mp4`.
- Other files keep a normalized detected extension.
- Files with no usable extension or MIME mapping use `.bin`.

The extension cannot be changed while renaming. Names are globally reserved,
including former names that redirect after a rename and names belonging to
deleted shares.

## Processing States

| State | Public-link behavior |
|---|---|
| Pending | Returns a `202` processing page |
| Ready | Redirects to the public B2 object |
| Failed | Returns a `503` unavailable page |
| Deleted | Returns `404` |

Only processed output is uploaded to B2. Non-video files are stored as staged.
Videos first attempt a fast stream-copy remux. Files that are not H.264/AAC are
transcoded to H.264/AAC MP4 with NVIDIA NVENC.

## History and Search

History is private to the signed-in owner and contains only current names.
Search matches the public slug, display filename, processing status, and
content type.

Former names created by renaming do not appear in history, but continue to
redirect publicly.

## Rename a Share

Select a share name in history, enter the replacement, and save it. The old URL
returns a permanent redirect to the newest name. Repeated renames flatten the
redirects so every former name points directly to the current one.

The old name remains reserved and cannot be assigned to another share.

## Delete a Share

Deleting a share:

- hides the current URL and every former redirect URL;
- cancels queued or running jobs for that share;
- removes known temporary staging files;
- retains metadata and open counters;
- permanently deletes the B2 object and thumbnail only when no active share
  still references them.

Deletion is reference-aware because identical uploads and processed
derivatives can share one stored object.

## Public Links and Previews

Normal requests to a ready `/s/{slug}` URL receive a `302` redirect to the
native public B2 URL. Application pods do not proxy the downloaded bytes.

Known unfurl crawlers receive a short-lived Open Graph page instead. Video
previews can include a player, dimensions, and an extracted JPEG thumbnail;
image shares expose the image directly through a stable media route.

Two supporting routes back these embeds:

- `/s/{slug}/media` redirects to the stored file and counts an open.
- `/s/{slug}/thumbnail` redirects to the JPEG thumbnail without counting.

The open count represents direct link redirects plus embed-proxy media fetches.
It is not a literal play or unique-view count because chat platforms cache
media.

## Security Model

Public links are intentionally unlisted and readable by anyone who knows the
URL. Do not use B2 Share for content that requires per-viewer authorization.

Creating, listing, renaming, and deleting shares requires a configured OIDC
role. Browser requests use signed sessions and CSRF protection; API clients can
use OIDC bearer tokens.

See the [API reference](api.md) for programmatic access.
