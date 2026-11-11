# mediacat task list

Open work beyond `PLAN.md` (whose Deferred section keeps the planned phase
work). Items come from real cleanups; finished ones are ticked, and a
finished section moves to `## Completed` at the end. Every item has a
priority, `(p1)` high, `(p2)` normal or `(p3)` low, and a category.

Categories: feature bug docs test perf

Reviewed: 2026-11-10

## 1. Acting on chosen groups without the review screen

From the 2026-11-10 Stash cleanup: the wanted action ("trash only video
copies outside Sorted, and inside Sorted keep the file Stash uses") could
not be expressed with flags, so 434 decisions were written into
`review_decisions` by hand and applied through `--review`.

- [x] **Keep every copy that matches the preferred paths.** (p1)
  [feature] `--keep-all-preferred` keeps all copies matching
  `preferred_paths` (and no avoided pattern) and acts on the rest, instead
  of letting later rules pick one. Chosen over leaving such groups
  untouched, which would also have kept their Incoming copies. With it,
  `dupes --trash` does the "only copies outside Sorted" pass directly.
- [x] **Apply saved review decisions without the review screen.** (p1)
  [feature] `dupes --review --apply-saved` (or `dupes --apply-decisions`):
  apply the saved choices for the groups found, with the same summary and
  confirmations as review (`--yes` for trash and move). Lets a script or
  another tool prepare decisions and apply them unattended.
- [ ] **Import decisions from a file.** (p2) [feature] `dupes --decisions
  FILE` (JSON or TSV: group hash, path to keep, action) loaded into
  `review_decisions`, so tools stop writing the table directly; reject rows
  whose group or kept path no longer matches the catalog.
- [x] **Limit actions to copies under a directory.** (p2) [feature]
  `--only-under DIR` (repeatable): trash, move or delete only copies below
  DIR, never others, whatever the keep rules choose. A guard for "clean up
  Incoming and Unsorted, never touch Sorted".
- [x] **Stash-aware keep rule.** (p2) [feature] A `stash` keep rule that
  prefers the copy Stash tracks, and above all a scene's primary file, read
  from the Stash database `stash_config` already points at; with a guard
  that never trashes a scene's primary file. In the cleanup, 79 groups had
  two copies in Sorted where only one was in Stash.
- [ ] **Group report by area.** (p3) [feature] A summary of the groups by
  where their copies are (`--summary-by DIR`, e.g. "kept copy in Sorted,
  others outside", "2+ copies in Sorted", "none in Sorted") with bytes, so
  a cleanup can be planned before choosing an action. This took a custom
  script over `--export` JSON.
- [x] **Stale "lowercased" comment on path patterns.** (p3) [docs]
  `internal/dupes/policy.go` `Policy.PathPrefer` still says the patterns
  are lowercased, but since d9674b3 they match case-sensitively; fix the
  comment so nobody reintroduces lowercasing.
- [ ] **Clear applied decisions in one statement.** (p3) [perf]
  `catalog.DeleteDecisions` runs one DELETE per key outside a transaction;
  for hundreds of applied groups (434 in the cleanup) batch them with
  `IN (...)` in chunks of 500, as `LoadDecisions` does.

## 2. Copying between drives

- [ ] **Copy files with no identical copy elsewhere.** (p2) [feature] A
  command that copies the files under a source path whose content has no
  copy on a target volume (full hash, as dupes), to a destination laid out
  by rules (keep the path below the source, or a mapping file), resumable,
  never overwriting (list name clashes instead), stopping before the
  target's free space drops under a margin, logged for undo. Done by hand
  on 2026-11-10 for a USB drive's performer folders (rsync and a plan TSV).
- [ ] **Survive drives that drop out.** (p2) [feature] scan, dupes hashing
  and actions on a removable drive: on EIO or ENODEV, wait for the device to
  come back, remount it (read-only when asked), and continue the file, not
  fail the run; a `--bwlimit` to read gently. A WD USB drive disconnected
  five times in three hours on 2026-11-10, under scans and copies.
- [ ] **Mount removable drives read-only for scans.** (p3) [feature] An
  option (or the default for scan and dupes, which only read) to remount a
  removable volume read-only first, so a drop-out cannot corrupt it.

## 3. Trash

- [ ] **Empty the trash from a script.** (p2) [feature] `trash --empty`
  refuses without an interactive terminal (by design). Add an explicit
  `--confirm delete` flag for scripted runs, still refusing without it,
  so an agent or a timer can empty it on a user's explicit request.
- [ ] **Empty only older batches.** (p3) [feature] `trash --empty
  --older-than 7d`: remove batches trashed more than N days ago and keep
  recent ones undoable, so space can be freed without losing the undo for
  today's cleanup.
- [ ] **Show what is in the trash by batch.** (p3) [feature] `trash` lists
  batches (date, command, files, bytes) next to the totals, as `history`
  does, so it is clear what emptying would remove.
