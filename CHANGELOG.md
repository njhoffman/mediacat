# Changelog

All notable changes to mediacat. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [semantic versioning](https://semver.org/) and are git tags
(`just release patch|minor|major`).

## [Unreleased]

## [0.14.0] - 2026-11-11

### Added

- `dupes --summary-by DIR` (repeatable): sorts the groups by where their
  copies are (kept copy inside with the others outside, several inside, kept
  copy outside with some inside, none inside) with the bytes the current keep
  rules and guards would reclaim and the bytes they spare, to choose flags
  before an action. Report only; `--json` prints it as JSON.

## [0.13.0] - 2026-11-11

### Added

- `mediacat decisions` lists the choices `dupes --review` saved;
  `decisions import FILE` loads them from TSV or JSON Lines (group key, path
  to keep, action), checking that each kept copy is cataloged and has the
  group's content, and importing nothing when any row is wrong;
  `decisions clear KEY...` or `--all` forgets them. `decisions list --json`
  prints the import format.

### Changed

- Applied decisions are cleared in one transaction instead of one statement
  per group.

## [0.12.0] - 2026-11-11

### Added

- `dupes --apply-saved`: apply the choices saved by `--review` without the
  review screen, for scripts. A dry run until `--execute`; `--yes` skips the
  confirmation for trash and move (never for delete). Groups without a saved
  choice are left alone, and a choice whose kept copy is no longer in its
  group is skipped with a warning. `--move-to` sets where saved moves go.

## [0.11.0] - 2026-11-11

### Added

- `stash` keep rule: keeps the file Stash uses, a scene's or image's primary
  file first, then any file Stash tracks (read-only from Stash's database).
- A scene's or image's primary file in Stash is never trashed, moved or
  deleted, whichever copy the keep rules choose; the summary lists such
  copies as left in place. When Stash's database exists but cannot be read,
  actions stop (`--stash-config ""` acts without Stash); reports only warn.

### Changed

- The default keep rules include `stash` after `paths`
  (`prefer,avoid,paths,stash,plain,oldest,shortest`, and for similar groups
  `res,bitrate,prefer,avoid,paths,stash,plain,size,oldest`). Unedited configs
  upgrade to the new template.

## [0.10.0] - 2026-11-11

### Added

- `dupes --keep-all-preferred`: keep every copy whose path matches a
  preferred pattern (`dupes.preferred_paths` / `--prefer-path`, without an
  avoided one) and act only on the rest, so a group with two copies in
  `/Sorted` loses only its copies elsewhere. Combines with `--only-under`.

## [0.9.0] - 2026-11-11

### Added

- Man pages: `mediacat(1)` and one page per command (`man mediacat-dupes`),
  generated from the `--help` text by the hidden `mediacat man DIR`.
- Shell completion for flag values: `--depth`, `--type`, `--similar`,
  `--keep` rules, `list --sort`/`--columns` fields and `--check` statuses
  (comma lists complete each element), directories for path arguments and
  directory flags, files for `info`, and recent batch numbers for `undo` and
  `history`. Free-text flags no longer offer file names.
- `just build` installs the man pages under `~/.local/share/man/man1` and the
  bash, zsh and fish completion scripts (`just install-docs` on its own).

## [0.8.0] - 2026-11-11

### Added

- `dupes --only-under DIR` (repeatable): trash, move or delete only copies below
  DIR, whichever copy the keep rules choose; the others stay in place and the
  summary says how many and how large. Also limits what `--review` applies.
- `TODO.md` with the work found while cleaning up a Stash library on
  2026-11-10: acting on chosen duplicate groups without the review screen,
  copying unique files between drives, drives that drop out, and scripted
  trash emptying.

## [0.7.0] - 2026-10-19

### Changed

- Preferred paths are case-sensitive, and the defaults are `/Sorted`,
  `^/Unsorted` and `^/Incoming`: a copy under `/Sorted` is kept, copies
  under `/Unsorted` or `/Incoming` are avoided, and an avoided match still
  outweighs a preferred one (`Sorted/x/Incoming` loses to an unmatched path
  but beats `Unsorted`). Unedited configs upgrade to the new template.
- `just release` also pushes the branch and the tag.

## [0.6.1] - 2026-10-19

### Fixed

- `goimagehash` and `imaging` are listed as direct dependencies in
  `go.mod`, so `go mod tidy` leaves them alone.

## [0.6.0] - 2026-10-19

### Added

- bpp (video bits per pixel per frame): a list field and column,
  `--min-bpp` / `--max-bpp`, an info row, JSON, and highlighting from 0.15
  to 0.4, to spot heavy encodes at any resolution.
- `--version` shows the git tag; `just release patch|minor|major` tags,
  rebuilds and links `~/.local/bin/mediacat` to the repo binary.

### Fixed

- Video phashes are sampled over the video stream's duration when the
  container's would put frames past the picture (audio longer than video).
  Every file Stash can hash still gets Stash's exact hash.
- ffprobe's MPEG-1/2 variable-bitrate marker (104857200) is no longer stored
  as a video bitrate; affected rows are re-derived from the stored ffprobe
  JSON.

## [0.5.0] - 2026-10-19

The first release: plan phases 1 to 5.

### Added

- `scan`: recursive, resumable cataloging into a SQLite catalog keyed by
  volume identity, with change and rename detection, ffprobe, tag and image
  metadata, average stream bitrates, and progress display. `list` and
  `info` with filters and a multi-key sort on every field, `--columns`, and
  yellow-to-red highlighting of large sizes, durations, resolutions and
  bitrates; a dashboard on bare `mediacat`; staleness warnings.
- `dupes`: tiered duplicate detection (size, sampled partial hash, full
  xxh3 hash) with hashes cached so interrupted runs resume; keep rules
  (prefer, avoid, preferred paths, plain names, age, path length,
  resolution, bitrate, size); trash on the files' own drive, move, or
  delete, as dry runs unless `--execute`, never overwriting and re-checking
  each file first; `history` and `undo`; `trash` to inspect and empty what
  mediacat trashed; reminders to run Stash's Clean.
- `dupes --review`: a full-screen list of the groups to choose the copy to
  keep and trash, move, delete or leave each, saving choices as they are
  made.
- `check`: integrity checks, quick (first frame and the data near the end)
  or `--full` (decode everything), stored per file and resumable; a list
  `--check` filter, a duplicate marker column and `--duplicates`.
- `dupes --similar`: perceptual duplicates of videos and images, with
  Stash's exact phash algorithm (Stash's stored phashes reused when path,
  size and modification time match), levels from exact to low, a duration
  window for videos, and their own keep rules; `--delete` only through
  `--review`.
- `~/.config/mediacat.yaml` with commented defaults, created on first run,
  and upgraded automatically when unedited.
