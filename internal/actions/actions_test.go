package actions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/volume"
)

type env struct {
	t     *testing.T
	dir   string
	db    *catalog.DB
	res   *volume.Resolver
	v     volume.Volume
	volID int64
	rel   string
	trash *Trasher
}

var t0 = time.Date(2025, 6, 1, 12, 0, 0, 0, time.Local)

func newEnv(t *testing.T) *env {
	t.Helper()
	res, err := volume.NewResolver()
	if err != nil {
		t.Skip(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	v, rel, err := res.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, _ := db.UpsertVolume(v, t0)
	tr := &Trasher{HomeTrash: filepath.Join(dir, ".home-trash"), HomeVolumeKey: v.Key, UID: os.Getuid()}
	return &env{t: t, dir: dir, db: db, res: res, v: v, volID: id, rel: rel, trash: tr}
}

func (e *env) write(rel, content string, day int) string {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	mt := t0.Add(time.Duration(day) * 24 * time.Hour)
	os.Chtimes(p, mt, mt)
	return p
}

// groups catalogs every file in dir and returns full-hash duplicate groups.
func (e *env) groups() []dupes.Group {
	e.t.Helper()
	tx, _ := e.db.Begin()
	filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(p, "trash") {
			return err
		}
		sub, _ := filepath.Rel(e.dir, p)
		f := catalog.File{VolumeID: e.volID, RelPath: filepath.ToSlash(filepath.Join(e.rel, sub)), Size: info.Size(),
			MtimeNS: info.ModTime().UnixNano(), Kind: filter.KindVideo, ScannedAt: t0}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			f.Inode = st.Ino
		}
		return tx.PutFile(f)
	})
	tx.Commit()
	files, err := e.db.Query(e.volID, e.rel, filter.Filter{MinSize: 1})
	if err != nil {
		e.t.Fatal(err)
	}
	pol, _ := dupes.ParsePolicy(dupes.DefaultPolicy, nil, nil)
	r, err := (&dupes.Finder{DB: e.db, Options: dupes.Options{Depth: dupes.DepthFull, Policy: pol}}).Find(context.Background(), files)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Groups
}

func (e *env) plan(kind, moveTo string) Plan {
	e.t.Helper()
	p, err := BuildPlan(kind, e.dir, moveTo, e.groups(), map[string]volume.Volume{e.v.Key: e.v})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) exec(p Plan) *Outcome {
	e.t.Helper()
	ex := &Executor{DB: e.db, Trasher: e.trash, Now: func() time.Time { return t0 }}
	out, err := ex.Execute(context.Background(), p)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (e *env) missingCount() int {
	fs, _ := e.db.Query(e.volID, e.rel, filter.Filter{Missing: true})
	return len(fs)
}

func TestTrashAndUndo(t *testing.T) {
	e := newEnv(t)
	keep := e.write("keep.mp4", "same content", 0)
	dup1 := e.write("sub/keep.mp4", "same content", 3) // same basename: name collision in trash
	dup2 := e.write("other/keep (1).mp4", "same content", 1)
	e.write("unique.mp4", "different!!!", 0)

	p := e.plan(catalog.ActionTrash, "")
	if len(p.Ops) != 2 || p.Bytes() != 24 {
		t.Fatalf("plan: %+v", p.Ops)
	}
	out := e.exec(p)
	if out.Done != 2 || len(out.Failed)+len(out.Skipped) != 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if !exists(keep) || exists(dup1) || exists(dup2) {
		t.Fatal("wrong files trashed")
	}
	files := filepath.Join(e.trash.HomeTrash, "files")
	for _, name := range []string{"keep.mp4", "keep (1).mp4"} {
		if !exists(filepath.Join(files, name)) {
			t.Errorf("%s not in trash", name)
		}
	}
	info, err := os.ReadFile(filepath.Join(e.trash.HomeTrash, "info", "keep.mp4.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(info), "Path="+escapePath(dup1)) || !strings.Contains(string(info), "DeletionDate=2025-06-01T12:00:00") {
		t.Errorf("trashinfo:\n%s", info)
	}
	if e.missingCount() != 2 {
		t.Errorf("trashed rows should be marked missing, got %d", e.missingCount())
	}
	u, _ := e.trash.Usage(e.trash.Locate(e.v))
	if u.Items != 2 || u.Bytes != 24 {
		t.Errorf("usage = %+v", u)
	}

	ex := &Executor{DB: e.db, Trasher: e.trash}
	undo, err := ex.Undo(context.Background(), out.BatchID)
	if err != nil || undo.Done != 2 {
		t.Fatalf("undo: %+v %v", undo, err)
	}
	if !exists(dup1) || !exists(dup2) || exists(filepath.Join(e.trash.HomeTrash, "info", "keep.mp4.trashinfo")) {
		t.Error("undo did not restore files and remove trashinfo")
	}
	if e.missingCount() != 0 {
		t.Error("undo should clear missing marks")
	}
	b, _, _ := e.db.GetBatch(out.BatchID)
	if b.UndoneAt == nil {
		t.Error("batch not marked undone")
	}
}

func TestTrashNameCollisionInsideTrash(t *testing.T) {
	e := newEnv(t)
	e.write("a/x.mp4", "1", 0)
	e.write("b/x.mp4", "1", 1)
	e.write("c/x.mp4", "1", 2)
	out := e.exec(e.plan(catalog.ActionTrash, ""))
	if out.Done != 2 {
		t.Fatalf("outcome %+v", out)
	}
	for _, n := range []string{"x.mp4", "x.2.mp4"} {
		if !exists(filepath.Join(e.trash.HomeTrash, "files", n)) {
			t.Errorf("missing trash entry %s", n)
		}
	}
}

func TestDeleteIsPermanent(t *testing.T) {
	e := newEnv(t)
	e.write("a.mp4", "dup", 0)
	b := e.write("b.mp4", "dup", 1)
	out := e.exec(e.plan(catalog.ActionDelete, ""))
	if out.Done != 1 || exists(b) {
		t.Fatalf("delete: %+v", out)
	}
	if all, _ := e.db.Query(e.volID, e.rel, filter.Filter{}); len(all) != 1 || e.missingCount() != 0 {
		t.Errorf("deleted row should be removed from the catalog")
	}
	if _, err := (&Executor{DB: e.db}).Undo(context.Background(), out.BatchID); err == nil {
		t.Error("undoing a delete should fail")
	}
}

func TestMoveSameDeviceAndUndo(t *testing.T) {
	e := newEnv(t)
	e.write("lib/a.mp4", "dup", 0)
	dup := e.write("lib/sub/b.mp4", "dup", 5)
	target := filepath.Join(e.dir, "holding")
	out := e.exec(e.plan(catalog.ActionMove, target))
	moved := filepath.Join(target, "lib/sub/b.mp4")
	if out.Done != 1 || exists(dup) || !exists(moved) {
		t.Fatalf("move: %+v", out)
	}
	if st, _ := os.Stat(moved); !st.ModTime().Equal(t0.Add(5 * 24 * time.Hour)) {
		t.Errorf("mtime not preserved: %v", st.ModTime())
	}
	undo, err := (&Executor{DB: e.db}).Undo(context.Background(), out.BatchID)
	if err != nil || undo.Done != 1 || !exists(dup) || exists(moved) {
		t.Errorf("undo move: %+v %v", undo, err)
	}
}

func TestMoveNeverOverwrites(t *testing.T) {
	e := newEnv(t)
	e.write("a.mp4", "dup", 0)
	dup := e.write("b.mp4", "dup", 1)
	target := filepath.Join(e.dir, "holding")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "b.mp4"), []byte("someone else's file"), 0o644)
	out := e.exec(e.plan(catalog.ActionMove, target))
	if out.Done != 0 || len(out.Failed) != 1 || !exists(dup) {
		t.Fatalf("overwrite case: %+v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "b.mp4")); string(b) != "someone else's file" {
		t.Error("existing destination was overwritten")
	}
}

func TestCrossDeviceMove(t *testing.T) {
	shm := "/dev/shm"
	if same, err := SameDevice(os.TempDir(), shm); err != nil || same {
		t.Skip("no second device available")
	}
	src := filepath.Join(t.TempDir(), "src.mp4")
	os.WriteFile(src, []byte("payload"), 0o644)
	mt := t0
	os.Chtimes(src, mt, mt)
	dstDir, err := os.MkdirTemp(shm, "mediacat-test-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dstDir) })

	// Wrong expected hash: refuse, keep the source, leave nothing behind.
	bad := filepath.Join(dstDir, "bad/out.mp4")
	if err := Move(context.Background(), src, bad, "0000"); err == nil || exists(bad) || exists(bad+".mediacat-partial") || !exists(src) {
		t.Fatalf("hash mismatch should abort cleanly: %v", err)
	}

	dst := filepath.Join(dstDir, "ok/out.mp4")
	if err := Move(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	if exists(src) {
		t.Error("source should be removed after a verified copy")
	}
	if b, _ := os.ReadFile(dst); string(b) != "payload" {
		t.Error("content mismatch")
	}
	if st, _ := os.Stat(dst); !st.ModTime().Equal(mt) {
		t.Errorf("mtime %v", st.ModTime())
	}
}

func TestSkipsWhenFilesChanged(t *testing.T) {
	e := newEnv(t)
	keep := e.write("a.mp4", "dup", 0)
	dup := e.write("b.mp4", "dup", 1)
	e.write("c.mp4", "zzz", 0)
	e.write("d.mp4", "zzz", 1)
	p := e.plan(catalog.ActionTrash, "")
	if len(p.Ops) != 2 {
		t.Fatalf("ops %d", len(p.Ops))
	}
	// Keeper of group 1 disappears; the duplicate in group 2 changes after hashing.
	os.Remove(keep)
	later := t0.Add(99 * 24 * time.Hour)
	for _, op := range p.Ops {
		if filepath.Base(op.File.AbsPath) == "d.mp4" {
			os.Chtimes(op.File.AbsPath, later, later)
		}
	}
	out := e.exec(p)
	if out.Done != 0 || len(out.Skipped) != 2 || !exists(dup) {
		t.Errorf("skips: %+v", out)
	}
	_, acts, _ := e.db.GetBatch(out.BatchID)
	if len(acts) != 2 || acts[0].Status != catalog.ActionSkipped {
		t.Errorf("actions log: %+v", acts)
	}
}

func TestInterruptedBatch(t *testing.T) {
	e := newEnv(t)
	e.write("a.mp4", "dup", 0)
	e.write("b.mp4", "dup", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := (&Executor{DB: e.db, Trasher: e.trash}).Execute(ctx, e.plan(catalog.ActionTrash, ""))
	if err != nil || !out.Interrupted || out.Done != 0 {
		t.Errorf("%+v %v", out, err)
	}
}

func TestBuildPlanRejectsUnconfirmed(t *testing.T) {
	g := dupes.Group{Files: []catalog.File{{}, {}}, Confirmed: false}
	if _, err := BuildPlan(catalog.ActionTrash, "/", "", []dupes.Group{g}, nil); err == nil {
		t.Error("unconfirmed groups must not produce a plan")
	}
	f := catalog.File{AbsPath: "/other/x.mp4", VolumeKey: "net://h/share", RelPath: "other/x.mp4"}
	if d := moveDest("/hold", "/lib", f); d != "/hold/net___h_share/other/x.mp4" {
		t.Errorf("outside-root dest = %s", d)
	}
}

func TestLocate(t *testing.T) {
	top := t.TempDir()
	tr := &Trasher{UID: 1000, HomeVolumeKey: "home", HomeTrash: "/home/x/.local/share/Trash", TopDir: func(volume.Volume) string { return top }}
	if l := tr.Locate(volume.Volume{Key: "home"}); l.Dir != "/home/x/.local/share/Trash" || l.TopDir != "" {
		t.Errorf("home: %+v", l)
	}
	if l := tr.Locate(volume.Volume{Key: "other"}); l.Dir != filepath.Join(top, ".Trash-1000") || l.Exists {
		t.Errorf("topdir: %+v", l)
	}
	os.Mkdir(filepath.Join(top, ".Trash"), 0o777|os.ModeSticky)
	os.Chmod(filepath.Join(top, ".Trash"), 0o777|os.ModeSticky)
	if l := tr.Locate(volume.Volume{Key: "other"}); l.Dir != filepath.Join(top, ".Trash", "1000") {
		t.Errorf("shared .Trash: %+v", l)
	}
}

func TestTopDirTrashUsesRelativePath(t *testing.T) {
	top, _ := filepath.EvalSymlinks(t.TempDir())
	f := filepath.Join(top, "dir with space", "a%b.mp4")
	os.MkdirAll(filepath.Dir(f), 0o755)
	os.WriteFile(f, []byte("x"), 0o644)
	tr := &Trasher{UID: 1000, TopDir: func(volume.Volume) string { return top }}
	loc := tr.Locate(volume.Volume{Key: "other"})
	_, info, err := tr.Trash(f, loc, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(info)
	if !strings.Contains(string(b), "Path=dir%20with%20space/a%25b.mp4\n") {
		t.Errorf("trashinfo:\n%s", b)
	}
}

func TestTrashRefusesOtherDevice(t *testing.T) {
	shm := "/dev/shm"
	if same, err := SameDevice(os.TempDir(), shm); err != nil || same {
		t.Skip("no second device available")
	}
	f := filepath.Join(t.TempDir(), "a.mp4")
	os.WriteFile(f, []byte("x"), 0o644)
	tr := &Trasher{UID: 1000, TopDir: func(volume.Volume) string { return shm }}
	loc := tr.Locate(volume.Volume{Key: "other"})
	if _, _, err := tr.Trash(f, loc, t0); err == nil || !strings.Contains(err.Error(), "different device") {
		t.Errorf("cross-device trash should be refused: %v", err)
	}
	if !exists(f) {
		t.Error("file must stay in place")
	}
}
