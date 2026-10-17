# mediacat

Interactive audio/image/video cataloger and duplicate manager. Scans any directory
recursively, keeps a persistent catalog of file and media metadata, finds duplicates at
selectable depths, and trashes, deletes, or moves them safely.

Status: planning. Phase 1 approved, not started.

## Goals

- Interactive, well-formatted output (color, aligned tables), progress bars for every
  long-running operation.
- Persistent catalog of file metadata plus media metadata (codecs, resolution, duration,
  bitrate, audio tags). Accessible after the program exits.
- Show when each scanned path was last updated; warn when it is stale.
- Duplicate detection with selectable method/depth; interruptible and resumable.
- Duplicate actions: trash, permanent delete, move to folder. Dry run by default.
- Duplicate report/export plus an interactive keep/act selector.
- Listing with size, duration, resolution, type and codec filters.
- Audio tag reading (ID3 and other formats).
- Integrity checking (valid vs corrupted media).

## Decisions

| Topic | Decision |
|---|---|
| Packaging | Standalone tool. `audit` integrates later by shelling out to `mediacat ... --json`. Follow audit conventions: dashboard on bare invocation, `--summary`, `--json`, staleness warnings. |
| Language | Go 1.25, module `github.com/njhoffman/mediacat`. |
| Scope of scan | Current directory by default, always recursive. Nothing excluded by default (including `Stash/`). |
| Volume identity | From `/proc/self/mountinfo`: network filesystems by mount source (e.g. `//192.168.1.248/share`), local filesystems by UUID. Paths stored relative to the volume root. No marker files written to drives. |
| Catalog location | `$XDG_DATA_HOME/mediacat/catalog.db` (local disk), so list/report work while a drive is unavailable. |
| Staleness | Warn when the newest scan covering a path is older than 30 days (configurable). |
| Filters | Individual flags, not an expression language. |
| Destructive actions | Dry run by default; `--execute` required. |
| Trash | Only on the drive the files live on (`<volume root>/.Trash-$UID`, freedesktop spec). Never fall back to cross-device trash; fail with a clear message instead. |
| Confirmation | Before any action, state the outcome prominently and color-coded: "N files (X GB) will be MOVED TO TRASH at <dir>" vs "N files (X GB) will be PERMANENTLY DELETED" (red, confirmed by typing `delete`). |
| Hardlinks | Same inode is reported separately, never acted on as a duplicate. |
| Symlinks | Not followed by default. |
| Missing files | Marked `missing_since`; pruned only with `--prune`. Unmounted volumes are marked unavailable, never pruned. |
| Stash | Read-only use of Stash's database. mediacat never writes to it. After actions affecting Stash library paths, remind the user to run Stash's Clean task. |
| Perceptual tiers | 3a (image phash) and Stash-compatible video phash in phase 5. Audio fingerprinting (3b) deferred. |

## Environment notes

- `/mnt/nfs` is an SMB/CIFS share (`//192.168.1.248/share`, `soft,serverino,nounix`),
  3.7T, ~95% full, almost entirely video. The server is a network drive appliance, so all
  hashing happens over the network (~9h for a full hash of 3.5T on gigabit). Tiered hashing
  and resumability are essential.
- `soft` mount with `retrans=1`: transient `EIO`/timeouts will occur. Integrity checks must
  record these as "unverified", never "corrupt", and retry.
- Trashing on the share is a fast server-side rename into `/mnt/nfs/.Trash-1000`, but frees no
  space until emptied. Show pending trash size and offer to empty it.
- Moves within the share are renames; moves off the share copy over the network. Check
  destination free space first.
- Stash (`~/.stash/config.yml`, DB `~/.stash/stash-go.sqlite`, schema 86) indexes only
  `/mnt/nfs/Stash/Sorted/*`. As of 2026-10-17: 3,759 of ~8,460 videos under `Sorted/` are
  known to Stash, 3,488 have a phash, nothing outside `Sorted/` is indexed. Stash phashes are
  therefore a cache, not a complete source.

## Dependencies

- `github.com/spf13/cobra` - CLI
- `github.com/charmbracelet/bubbletea`, `bubbles`, `lipgloss` - TUI, progress, tables, styling
- `modernc.org/sqlite` - pure-Go SQLite (no cgo)
- `github.com/dhowden/tag` - audio tags (read-only)
- `github.com/zeebo/xxh3` - content hashing
- `github.com/corona10/goimagehash` - perceptual hashing (phase 5)
- External: `ffprobe`, `ffmpeg`

## Layout

```
cmd/mediacat/        main
internal/volume/     path -> volume (mount source, fstype, root); trash dir resolution
internal/catalog/    SQLite schema, migrations, queries
internal/scan/       recursive walk, change detection, probe worker pool
internal/probe/      ffprobe JSON, image header, audio tags -> one struct
internal/filter/     size/duration/resolution/type/codec flag parsing
internal/ui/         lipgloss styles, progress model, tables, staleness banner
internal/cli/        cobra commands
```

## Phase 1: scan, catalog, list

### Schema

- `volumes`: id, source, fstype, uuid, last_mount_path, last_seen_at
- `files`: id, volume_id, relpath, size, mtime_ns, inode, kind (video/audio/image),
  container, duration_ms, width, height, video_codec, audio_codec, bitrate, fps,
  sample_rate, channels, tag_artist, tag_album, tag_title, tag_track, tag_year, tag_genre,
  probe_json, probe_error, scanned_at, missing_since. Unique (volume_id, relpath).
- `scans`: id, volume_id, root_relpath, started_at, finished_at, files_seen, added,
  changed, removed, status (running/complete/interrupted)

WAL mode. Schema versioned with an embedded migration list.

### Commands

- `mediacat scan [path]`
  - Recursive from `path` (default `.`).
  - Re-probes only when size or mtime changed; inode match used as a rename hint.
  - Probe worker pool, default 2 workers on rotational/network volumes, more on SSD;
    `--workers` overrides.
  - Progress bar with count, throughput and ETA.
  - Ctrl-C commits completed work and marks the scan `interrupted`; re-running continues.
  - Files no longer on disk get `missing_since`; `--prune` removes them.
- `mediacat list [path]`
  - `--type video|audio|image`
  - `--min-size` / `--max-size` (`500M`, `2G`)
  - `--min-duration` / `--max-duration` (`5m`, `1:30:00`)
  - `--min-res` / `--max-res` (`720p`, `1080p`, `4k`, `1920x1080`)
  - `--codec`, `--ext`, `--sort`, `--limit`, `--missing`, `--json`
- `mediacat info <file>` - full metadata for one file.
- `mediacat` (no args) - dashboard: per-volume file counts, total size, type breakdown,
  last scan time, staleness banner.

### Tests

- Table-driven unit tests: filter parsing, volume resolution against fixture mountinfo,
  change detection.
- Integration tests against tiny media generated with `ffmpeg -f lavfi` in a temp dir
  (video, audio with tags, image, plus a truncated file).

## Phase 2: exact duplicates and actions

- Tiers: size -> partial hash (first/middle/last 64K) -> full hash (xxh3). Selectable with
  a depth flag.
- Hash cache keyed by (volume_id, relpath, size, mtime_ns). Reuse Stash oshash where the
  file's size and mtime match Stash's record.
- Jobs table checkpoints stage and remaining work; Ctrl-C finishes the current file,
  commits, exits. Re-run resumes.
- Keep policies, composable: highest resolution, highest bitrate, oldest mtime, shortest
  path, preferred-directory list.
- Actions: trash (same volume only), permanent delete, move (preserve relative structure
  under target). Dry run default, `--execute` to apply, prominent confirmation as above.
- Action log table for undoing moves.
- Optional sidecar handling (`.srt`, `.xmp`, `.nfo`, `.cue`) behind a flag.
- Export duplicate groups to CSV and JSON.
- Stash Clean reminder after actions touching Stash library paths.

## Phase 3: interactive duplicate review

- Bubbletea TUI over duplicate groups: side-by-side metadata, keep-policy preselection,
  per-file override, per-group action, summary and confirmation before applying.

## Phase 4: integrity check

- Quick: ffprobe parses, container vs stream duration agree, decode near the end succeeds.
  Images: full decode.
- Full: `ffmpeg -v error -i f -f null -`, resumable like duplicate jobs.
- Network errors recorded as unverified and retried, never reported as corruption.

## Phase 5: perceptual duplicates

- 3a: image pHash/dHash with Hamming threshold.
- Video: reproduce Stash's phash algorithm (25 frames tiled 5x5, `goimagehash`
  PerceptionHash, 64-bit) so values are directly comparable. Use Stash's stored phash when
  the file's size and mtime match Stash's record; compute otherwise.
- Similarity levels by Hamming distance (exact/high/medium/low), mirroring Stash's.

## Deferred

- 3b: audio duplicates via tags + duration tolerance, optional chromaprint (`fpcalc` not
  installed; `libchromaprint-tools`).
- `audit files media` integration.
