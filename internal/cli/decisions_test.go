package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// groupHash returns the key of the fixture's one duplicate group, from dupes --json.
func groupHash(t *testing.T, h *harness) string {
	t.Helper()
	out, _ := h.ok("dupes", h.dir, "--json")
	var groups []struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(out), &groups); err != nil || len(groups) != 1 {
		t.Fatalf("dupes --json: %v %s", err, out)
	}
	return groups[0].Hash
}

func TestDecisionsImportApply(t *testing.T) {
	h, paths := dupeFixture(t)
	key := groupHash(t, h)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o644)
		return p
	}

	out, _ := h.ok("decisions")
	if !strings.Contains(out, "No saved decisions") {
		t.Errorf("empty list:\n%s", out)
	}

	// Every bad row is reported and nothing is imported.
	bad := write("bad.tsv", strings.Join([]string{
		"group\tkeep\taction",
		key + "\t" + paths[1] + "\tshred",
		"0000\t" + paths[1] + "\ttrash",
		"other\tbackup/clip.mp4\ttrash",
		"third\t" + filepath.Join(h.dir, "nope.mp4") + "\ttrash",
		key + "\t" + paths[1] + "\ttrash",
		key + "\t" + paths[2] + "\ttrash",
		"short\tline",
	}, "\n")+"\n")
	_, errs, code := h.run("decisions", "import", bad)
	if code != 1 || !strings.Contains(errs, "line 8: want 3 tab-separated columns") {
		t.Errorf("malformed line: %d %s", code, errs)
	}
	os.WriteFile(bad, []byte(strings.Join(strings.Split(string(mustRead(t, bad)), "\n")[:7], "\n")+"\n"), 0o644)
	_, errs, code = h.run("decisions", "import", bad)
	for _, want := range []string{"line 2: unknown action", "line 3: ", "does not have the content", "line 4: the copy to keep must be an absolute path",
		"line 5: ", "nope.mp4 does not exist", "line 7: group", "also decided on line 6", "5 of 6 rows rejected; nothing was imported"} {
		if !strings.Contains(errs, want) {
			t.Errorf("rejections missing %q:\n%s", want, errs)
		}
	}
	if code != 1 {
		t.Errorf("rejected import exited %d", code)
	}
	if out, _ := h.ok("decisions"); !strings.Contains(out, "No saved decisions") {
		t.Errorf("a rejected file must import nothing:\n%s", out)
	}

	// A good JSON Lines file, applied unattended.
	good := write("good.jsonl", fmt.Sprintf(`{"group": %q, "keep": %q, "action": "trash"}`+"\n", key, paths[1]))
	out, _ = h.ok("decisions", "import", good)
	if !strings.Contains(out, "Imported 1 decisions") {
		t.Errorf("import:\n%s", out)
	}
	out, _ = h.ok("decisions", "list")
	if !strings.Contains(out, "1 trash") || !strings.Contains(out, paths[1]) || !strings.Contains(out, key) {
		t.Errorf("list:\n%s", out)
	}
	out, _ = h.ok("decisions", "list", "--json")
	var row decisionRow
	if err := json.Unmarshal([]byte(out), &row); err != nil || row.Group != key || row.Keep != paths[1] || row.Action != "trash" {
		t.Errorf("list --json: %v %s", err, out)
	}
	// The listing re-imports as is (a round trip), here from stdin.
	h.stdin = out
	h.ok("decisions", "import", "-")
	h.stdin = ""
	out, _ = h.ok("dupes", h.dir, "--apply-saved", "--execute", "--yes")
	if !strings.Contains(out, "Done: 2 files") || !exists(paths[1]) || exists(paths[0]) || exists(paths[2]) {
		t.Fatalf("apply imported:\n%s", out)
	}
}

func TestDecisionsClear(t *testing.T) {
	h, paths := dupeFixture(t)
	key := groupHash(t, h)
	tsv := filepath.Join(t.TempDir(), "d.tsv")
	os.WriteFile(tsv, []byte(key+"\t"+paths[0]+"\tleave\n"), 0o644)
	h.ok("decisions", "import", tsv)

	if _, errs, code := h.run("decisions", "clear"); code != 1 || !strings.Contains(errs, "or --all") {
		t.Errorf("clear without keys: %d %s", code, errs)
	}
	if _, errs, code := h.run("decisions", "clear", "nope"); code != 1 || !strings.Contains(errs, "no saved decision for nope") {
		t.Errorf("clear unknown key: %d %s", code, errs)
	}
	if out, _ := h.ok("decisions", "clear", key); !strings.Contains(out, "Forgot 1 decisions") {
		t.Errorf("clear key: %s", out)
	}
	h.ok("decisions", "import", tsv)
	if _, errs, code := h.run("decisions", "clear", "--all"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Errorf("clear --all needs a terminal or --yes: %d %s", code, errs)
	}
	if out, _ := h.ok("decisions", "clear", "--all", "--yes"); !strings.Contains(out, "Forgot 1 decisions") {
		t.Errorf("clear --all: %s", out)
	}
	if got, _ := h.complete("decisions", "clear", ""); len(got) != 0 {
		t.Errorf("no keys left to complete: %q", got)
	}

	// A file that exists but was never scanned is rejected.
	fresh := filepath.Join(h.dir, "fresh.mp4")
	os.WriteFile(fresh, mustRead(t, paths[0]), 0o644)
	os.WriteFile(tsv, []byte(key+"\t"+fresh+"\ttrash\n"), 0o644)
	if _, errs, code := h.run("decisions", "import", tsv); code != 1 || !strings.Contains(errs, "fresh.mp4 is not in the catalog") {
		t.Errorf("uncataloged file: %d %s", code, errs)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
