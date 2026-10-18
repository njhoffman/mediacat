# mediacat

Interactive audio/image/video cataloger and duplicate manager. Scans any directory
recursively, keeps a persistent catalog of file and media metadata, finds duplicates at
selectable depths, and trashes, deletes, or moves them safely.

Status: phases 1-4 implemented (phase 4 uncommitted, pending review). Phase 5 not started.

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

### Implementation notes (phase 1)

Decisions made during implementation, beyond the plan above:

- Trash and NAS metadata directories are always skipped: `.Trash`, `.Trash-*`, `.Trashes`, `trashbox`,
  `$RECYCLE.BIN`, `System Volume Information`, `@eaDir`, `#recycle`, `lost+found`.
  Otherwise trashed files would be cataloged and later reported as duplicates.
- `--exclude` globs match file and directory base names. Excluded or unreadable paths are
  never marked missing, so narrowing a scan does not flag files as deleted.
- New files are written to the catalog only after probing, so an interrupted scan resumes by
  re-probing just the unfinished files. Probe results commit every 50 files or 2 seconds.
- Probe failures are split: permanent errors (not valid media) are recorded with
  `probe_error`; transient ones (EIO, timeouts, dropped connection) are reported as deferred
  and not recorded, so the next scan retries them.
- Renames are detected only when exactly one missing file has the same (inode, size, mtime).
- Scans record their PID; a scan left `running` by a dead process is shown as interrupted.
- Extra flags: `scan --probe-timeout`, `scan -j/--workers`, `list --errors`, `list --all`,
  `info --json` (includes raw ffprobe output). `--stale-days 0` disables staleness warnings.
- Progress goes to stderr (stdout stays clean for `--json`). Non-terminal output logs one line
  per phase plus one every 5 seconds.

Measured on the share (2 workers): walking ~80 files/s (one lstat round trip per file),
probing ~7 files/s. A first scan of the full share (~11k media files) should take roughly
25-30 minutes; rescans of unchanged trees take only the walk.

## Phase 2: exact duplicates and actions

### Commands

- `mediacat dupes [path]` - find duplicates under path (or `--all`), report groups with the
  copy to keep marked, and optionally act on the extra copies.
  - `--depth size|partial|full` (default full). size and partial are report-only.
  - `--type`, `--min-size` (default 1 byte, so empty files never match).
  - `--keep RULES` (default `prefer,avoid,plain,oldest,shortest`), `--prefer DIR`, `--avoid DIR`.
    Rules: prefer, avoid, plain (fewest copy markers like "(1)", " - Copy", "_2"), oldest,
    newest, shortest, longest; ties go to the lexically first path.
  - `--trash` | `--move-to DIR` | `--delete`, dry run unless `--execute`.
  - `--yes` skips the y/N prompt for trash and move only; `--delete` always requires typing
    `delete` in a terminal.
  - `--json`, `--export FILE.csv|.json`, `--limit`, `-j/--workers`, `--dry-run-files`.
- `mediacat history [batch]` - past batches, or the files in one.
- `mediacat undo <batch>` - restore a trash or move batch.
- `mediacat trash [path]` - trash location, size, and how much mediacat put there;
  `--empty` purges mediacat's entries, `--empty --everything` purges the whole trash. Both
  require typing `delete`.

### Implementation notes (phase 2)

- No separate jobs table. Hashes live on the `files` row with the size/mtime they were
  computed at; they are valid only while those match. An interrupted run commits finished
  hashes (every 50 files or 2s) and a re-run hashes only what is missing. This gives the
  planned resumability without job state to reconcile.
- Stash oshash reuse was dropped: the partial hash costs three 64 KiB reads per file and
  oshash cannot be compared against it, so the saving was not worth the mixed-hash logic.
- Hash: xxh3-128. Partial = size + first/middle/last 64 KiB (files <= 192 KiB are hashed
  whole). Workers default to 2 on network volumes, 1 on spinning disks, 4 otherwise.
- Hardlinks (same volume and inode) are collapsed before grouping and never acted on.
- Files whose size/mtime differ from the catalog are skipped as "changed since last scan".
- Right before acting on each file, mediacat re-checks that it and its kept copy exist,
  are unchanged since hashing, and are not the same file. Otherwise the file is skipped.
- Trash follows the freedesktop spec: home trash for files on the home volume, else
  `$top/.Trash/$uid` (sticky shared dir) or `$top/.Trash-$uid`, with `$top` the mount point.
  Before any prompt, every file is checked to be on the same device as its trash; a
  mismatch aborts the whole action. `.trashinfo` is written first (O_EXCL) to reserve the
  name, as the spec requires; paths are relative to `$top` for volume trashes.
- Renames use `renameat2(RENAME_NOREPLACE)`, falling back to check-then-rename where the
  filesystem does not support it. Nothing is ever overwritten.
- Cross-device moves check free space, copy to `<dest>.mediacat-partial` while hashing,
  verify the hash, fsync, preserve mtime, rename into place, then remove the source.
- Trashed and moved files are marked missing in the catalog (undo clears it without
  re-probing); deleted files are removed. Every operation is logged in `actions`.
- Stash: library paths are read from `--stash-config` (default `~/.stash/config.yml`).
  After an action or undo touching them, mediacat reminds you to run Clean in Stash.

Measured on the share: full hashing ran at ~37 MB/s with 2 workers (6 large candidates,
3.5 minutes); cached re-runs take well under a second.

## Config, list fields, bitrates (between phases 2 and 3)

- Config at `$XDG_CONFIG_HOME/mediacat.yaml` (`--config`, `MEDIACAT_CONFIG`), written with
  commented defaults on first run. Precedence: flag, env, config, built-in. Missing keys keep
  their defaults; unknown keys are errors.
- `list`: filters for every field (bitrates, fps, year, codecs, container, name/path/tag
  text, modified date), multi-key `--sort` with "-" for descending (unknown values last),
  and `--columns` (full list, or +field/-field adjustments; sorted fields are added unless
  the columns are given outright). Defaults come from `list.columns` / `list.audio_columns`.
- Size, duration and resolution cells shade pale yellow to bold red between
  `list.highlight.*.warn` and `.max` (defaults 500M-4G, 30m-2h, 1920x1080-3840x2160 by
  pixel count).
- Dupes keep rule `paths` with `dupes.preferred_paths` (default `/sorted`, `^incoming`):
  case-insensitive substrings, "^" avoids; an avoided match outweighs a preferred one.
- Per-stream video and audio bitrates are recorded (stream `bit_rate`, else mkvmerge `BPS`
  tags, else derived from the container rate). Upgrading backfills them from stored ffprobe
  JSON; files whose streams report no rate at all stay unknown until re-probed.

## Phase 3: interactive duplicate review

- `mediacat dupes [path] --review` (`just review [path]`): full-screen list of every
  confirmed group. Each copy shows date, resolution, duration, video codec and bitrate.
- Keys: j/k move, ]/[ jump groups, space keeps the copy under the cursor, t/m/d set the
  group to trash/move/delete the other copies, s leaves it, T/M/D/S do the same for every
  group, o opens the copy with xdg-open, a applies, q quits, ? shows help.
- The initial keeper comes from the keep policy; the initial action from --trash/--delete/
  --move-to if given, otherwise "leave".
- Choices are saved on every change in `review_decisions`, keyed by the group's content
  hash, so quitting and re-running resumes them (as long as the group is unchanged).
  Applied groups' choices are cleared.
- Apply runs one batch per action kind (trash, then move, then delete) through the normal
  confirmation: y/N for trash and move, typed `delete` for delete. Move uses --move-to or
  `dupes.move_to` from the config; without either, move is unavailable in the review.
- Multi-key bursts (fast typing) are split into individual keys.

## Phase 4: integrity check

- `mediacat check [path]` (`just check-media`), `--full`, `--recheck`, `--type`, `--json`,
  `-j`. Defaults in the config `check:` section.
- Statuses: ok, damaged (decode errors or missing data), unreadable (not valid media or
  nothing decodes), unverified (network error or timeout; retried next run, never reported
  as damage). Stored per file with the size/mtime they apply to; a full result also counts
  for quick runs. Ctrl-C keeps finished results.
- Quick check: decode the first frame (always a keyframe, so cheap), then read packets
  without decoding from 3s before the end; "partial file" or data ending more than 2s
  short of the duration means truncated. Damage in the middle of a file needs `--full`.
- Full check: `ffmpeg -f null` decode of the whole file with byte progress from
  `-progress`; error count (first 200 lines kept) and first message recorded.
- Images: full decode in Go (jpeg, png, gif, webp, bmp, tiff), ffmpeg for other formats.
- `list --check damaged,unreadable`, `check` column/field.
- Measured on Stash/Enhanced (578 files over SMB, 4 workers): 8.3 minutes, 2 real problems.
  An earlier design (decode one second at the end with an accurate seek) took ~3.3 s and
  ~6.5 CPU-seconds per file and produced false positives: seek-point reference errors and
  "truncated" files that decode completely. Keyframe-only decoding (-skip_frame) was also
  rejected: it misses truncation after the last keyframe and makes HEVC warn spuriously.

## List additions after phase 4

- Duplicate marker column `dup` (default first column, hidden when no listed file has one):
  files whose current full hash matches another present file (hardlinks once). `--duplicates`
  filter and `dup` sort field (by number of copies).
- `vbitrate` in the default columns and highlighted (default 10M-50M).
- Heat colors are never bold; 64-step gradient on 24-bit color terminals, 10 otherwise.
- Unedited config files from an earlier release are upgraded in place to the current
  template (identified by SHA-256 in `previousTemplates`).

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
