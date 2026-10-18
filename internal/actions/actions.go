package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/hash"
	"github.com/njhoffman/mediacat/internal/volume"
	"golang.org/x/sys/unix"
)

// Op is one redundant copy to act on.
type Op struct {
	File   catalog.File
	Keeper catalog.File
	Dest   string // move destination
}

// Plan is a set of operations of one kind.
type Plan struct {
	Kind    string
	Root    string // directory the duplicates were searched under
	MoveTo  string
	Ops     []Op
	Volumes map[string]volume.Volume // by key, for files in the plan
	Similar bool                     // some groups are perceptual matches, not identical copies
}

// Bytes is the total size of the files the plan touches.
func (p Plan) Bytes() int64 {
	var n int64
	for _, op := range p.Ops {
		n += op.File.Size
	}
	return n
}

// BuildPlan turns confirmed duplicate groups into operations on every non-kept copy.
// Unconfirmed groups (size or partial matches) are never acted on.
func BuildPlan(kind, root, moveTo string, groups []dupes.Group, vols map[string]volume.Volume) (Plan, error) {
	p := Plan{Kind: kind, Root: root, MoveTo: moveTo, Volumes: vols}
	for _, g := range groups {
		if !g.Confirmed {
			return p, errors.New("actions need a full-hash comparison (--depth full)")
		}
		p.Similar = p.Similar || g.Similar
		keeper := g.Files[g.Keep]
		for i, f := range g.Files {
			if i == g.Keep {
				continue
			}
			op := Op{File: f, Keeper: keeper}
			if kind == catalog.ActionMove {
				op.Dest = moveDest(moveTo, root, f)
			}
			p.Ops = append(p.Ops, op)
		}
	}
	return p, nil
}

// moveDest keeps the file's layout below root under moveTo; files outside root keep their
// volume-relative layout under a directory named for the volume.
func moveDest(moveTo, root string, f catalog.File) string {
	if rel, err := filepath.Rel(root, f.AbsPath); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(moveTo, rel)
	}
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(f.VolumeKey)
	return filepath.Join(moveTo, safe, f.RelPath)
}

// Event reports execution progress.
type Event struct {
	Done       int
	Total      int
	Bytes      int64
	TotalBytes int64
	Current    string
}

// Outcome summarizes an executed batch.
type Outcome struct {
	BatchID     int64
	Done        int
	Bytes       int64
	Failed      []dupes.Failure
	Skipped     []dupes.Failure
	Interrupted bool
	Paths       []string // source paths of completed operations
}

// Executor applies plans.
type Executor struct {
	DB       *catalog.DB
	Trasher  *Trasher
	Now      func() time.Time
	Progress func(Event)
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

var errSkip = errors.New("skip")

// verify re-checks a file and its keeper right before acting, so nothing is removed on
// the strength of a stale catalog: the file must be unchanged since it was hashed and the
// keeper must still exist, be unchanged, and be a different file.
func verify(op Op) error {
	st, err := os.Stat(op.File.AbsPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errSkip, err)
	}
	if st.Size() != op.File.Size || st.ModTime().UnixNano() != op.File.MtimeNS {
		return fmt.Errorf("%w: changed since it was hashed", errSkip)
	}
	kst, err := os.Stat(op.Keeper.AbsPath)
	if err != nil {
		return fmt.Errorf("%w: kept copy %s is gone", errSkip, op.Keeper.AbsPath)
	}
	if kst.Size() != op.Keeper.Size || kst.ModTime().UnixNano() != op.Keeper.MtimeNS {
		return fmt.Errorf("%w: kept copy %s changed since it was hashed", errSkip, op.Keeper.AbsPath)
	}
	if os.SameFile(st, kst) {
		return fmt.Errorf("%w: same file as the kept copy", errSkip)
	}
	return nil
}

// Execute runs the plan, logging each operation. A cancelled ctx stops after the current
// file and marks the batch interrupted.
func (e *Executor) Execute(ctx context.Context, p Plan) (*Outcome, error) {
	batch, err := e.DB.StartBatch(p.Kind, p.Root, p.MoveTo, e.now())
	if err != nil {
		return nil, err
	}
	out := &Outcome{BatchID: batch}
	ev := Event{Total: len(p.Ops), TotalBytes: p.Bytes()}
	for _, op := range p.Ops {
		if ctx.Err() != nil {
			out.Interrupted = true
			break
		}
		a := catalog.Action{BatchID: batch, VolumeKey: op.File.VolumeKey, RelPath: op.File.RelPath,
			SourcePath: op.File.AbsPath, Size: op.File.Size, FullHash: op.File.FullHash,
			KeeperPath: op.Keeper.AbsPath, Status: catalog.ActionDone}
		err := verify(op)
		if err == nil {
			a.DestPath, a.InfoPath, err = e.apply(ctx, p, op)
		}
		a.At = e.now()
		switch {
		case errors.Is(err, errSkip):
			a.Status, a.Error = catalog.ActionSkipped, strings.TrimPrefix(err.Error(), errSkip.Error()+": ")
			out.Skipped = append(out.Skipped, dupes.Failure{Path: op.File.AbsPath, Err: a.Error})
		case err != nil:
			a.Status, a.Error = catalog.ActionFailed, err.Error()
			out.Failed = append(out.Failed, dupes.Failure{Path: op.File.AbsPath, Err: a.Error})
		default:
			out.Done++
			out.Bytes += op.File.Size
			out.Paths = append(out.Paths, op.File.AbsPath)
		}
		if err := e.DB.RecordAction(a, op.File.ID, p.Kind); err != nil {
			return out, fmt.Errorf("recording action (the file operation itself completed): %w", err)
		}
		ev.Done++
		ev.Bytes += op.File.Size
		ev.Current = op.File.AbsPath
		if e.Progress != nil {
			e.Progress(ev)
		}
	}
	status := catalog.ScanComplete
	if out.Interrupted {
		status = catalog.ScanInterrupted
	}
	return out, e.DB.FinishBatch(batch, status, e.now())
}

func (e *Executor) apply(ctx context.Context, p Plan, op Op) (dest, info string, err error) {
	switch p.Kind {
	case catalog.ActionDelete:
		return "", "", os.Remove(op.File.AbsPath)
	case catalog.ActionTrash:
		v, ok := p.Volumes[op.File.VolumeKey]
		if !ok {
			return "", "", fmt.Errorf("volume %s is not mounted", op.File.VolumeKey)
		}
		return e.Trasher.Trash(op.File.AbsPath, e.Trasher.Locate(v), e.now())
	case catalog.ActionMove:
		return op.Dest, "", Move(ctx, op.File.AbsPath, op.Dest, op.File.FullHash)
	}
	return "", "", fmt.Errorf("unknown action %q", p.Kind)
}

// Move moves src to dst without ever overwriting. Within a device it renames; across
// devices it checks free space, copies while hashing, verifies the hash (when known),
// preserves the modification time, and only then removes src.
func Move(ctx context.Context, src, dst, wantHash string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination %s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := renameNoReplace(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if free, err := FreeBytes(filepath.Dir(dst)); err == nil && free < uint64(st.Size()) {
		return fmt.Errorf("not enough space at %s: need %d bytes, %d free", filepath.Dir(dst), st.Size(), free)
	}
	tmp := dst + ".mediacat-partial"
	if err := copyVerified(ctx, src, tmp, wantHash); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := renameNoReplace(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}

// renameNoReplace renames atomically without overwriting an existing dst. Filesystems
// that do not support RENAME_NOREPLACE (some network filesystems) fall back to a check
// followed by a plain rename.
func renameNoReplace(src, dst string) error {
	err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("destination %s already exists", dst)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		if _, err := os.Lstat(dst); err == nil {
			return fmt.Errorf("destination %s already exists", dst)
		}
		return os.Rename(src, dst)
	}
	return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
}

func copyVerified(ctx context.Context, src, dst, wantHash string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	h := hash.Writer()
	err = hash.Copy(ctx, multiWriter{out, h}, in, nil)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if wantHash != "" && h.Sum() != wantHash {
		return fmt.Errorf("content of %s no longer matches its hash; not moved", src)
	}
	return nil
}

type multiWriter struct {
	a *os.File
	b *hash.Hasher
}

func (m multiWriter) Write(p []byte) (int, error) {
	n, err := m.a.Write(p)
	if err != nil {
		return n, err
	}
	return m.b.Write(p[:n])
}

// FreeBytes returns the space available to unprivileged users at path.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// SameDevice reports whether two existing paths are on the same device.
func SameDevice(a, b string) (bool, error) {
	da, err := deviceOf(a)
	if err != nil {
		return false, err
	}
	db, err := deviceOf(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// Undo reverts a trash or move batch: each file goes back to its original path unless
// something now occupies it. Deletions cannot be undone.
func (e *Executor) Undo(ctx context.Context, batchID int64) (*Outcome, error) {
	b, acts, err := e.DB.GetBatch(batchID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("no batch %d", batchID)
	}
	if b.Kind == catalog.ActionDelete {
		return nil, errors.New("permanently deleted files cannot be restored")
	}
	out := &Outcome{BatchID: batchID}
	ev := Event{}
	for _, a := range acts {
		if a.Status == catalog.ActionDone {
			ev.Total++
			ev.TotalBytes += a.Size
		}
	}
	for _, a := range acts {
		if a.Status != catalog.ActionDone {
			continue
		}
		if ctx.Err() != nil {
			out.Interrupted = true
			break
		}
		err := undoOne(ctx, a)
		switch {
		case err != nil:
			out.Failed = append(out.Failed, dupes.Failure{Path: a.SourcePath, Err: err.Error()})
		default:
			if err := e.DB.MarkActionUndone(a); err != nil {
				return out, err
			}
			out.Done++
			out.Bytes += a.Size
			out.Paths = append(out.Paths, a.SourcePath)
		}
		ev.Done++
		ev.Bytes += a.Size
		ev.Current = a.SourcePath
		if e.Progress != nil {
			e.Progress(ev)
		}
	}
	if len(out.Failed) == 0 && !out.Interrupted {
		return out, e.DB.MarkBatchUndone(batchID, e.now())
	}
	return out, nil
}

func undoOne(ctx context.Context, a catalog.Action) error {
	if _, err := os.Lstat(a.SourcePath); err == nil {
		return fmt.Errorf("original path is occupied")
	}
	if _, err := os.Stat(a.DestPath); err != nil {
		return fmt.Errorf("no longer at %s", a.DestPath)
	}
	if err := Move(ctx, a.DestPath, a.SourcePath, a.FullHash); err != nil {
		return err
	}
	if a.InfoPath != "" {
		os.Remove(a.InfoPath)
	}
	return nil
}
