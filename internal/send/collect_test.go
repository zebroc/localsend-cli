package send

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCollectSingleFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "report.pdf"), "content")

	entries, err := Collect([]string{filepath.Join(dir, "report.pdf")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	// A plain file is sent under its base name, without any directory prefix.
	if entries[0].Name != "report.pdf" {
		t.Errorf("name = %q, want %q", entries[0].Name, "report.pdf")
	}
	if entries[0].Size != int64(len("content")) {
		t.Errorf("size = %d, want %d", entries[0].Size, len("content"))
	}
	if entries[0].Modified.IsZero() {
		t.Error("modification time was not filled in")
	}
}

// A directory is transferred as files carrying the relative path below it, with
// the directory's own name as the first segment.
func TestCollectDirectoryKeepsStructure(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tree", "a.txt"), "a")
	writeFile(t, filepath.Join(dir, "tree", "sub", "b.txt"), "b")
	writeFile(t, filepath.Join(dir, "tree", "sub", "deep", "c.txt"), "c")

	entries, err := Collect([]string{filepath.Join(dir, "tree")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{"tree/a.txt", "tree/sub/b.txt", "tree/sub/deep/c.txt"}
	if got := names(entries); !equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name, `\`) {
			t.Errorf("name %q must use forward slashes", entry.Name)
		}
	}
}

func TestCollectTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tree", "a.txt"), "a")

	withSlash, err := Collect([]string{filepath.Join(dir, "tree") + string(filepath.Separator)})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := names(withSlash); !equal(got, []string{"tree/a.txt"}) {
		t.Errorf("names = %v, want [tree/a.txt]", got)
	}
}

func TestCollectMixedArguments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "loose.txt"), "loose")
	writeFile(t, filepath.Join(dir, "tree", "a.txt"), "a")

	entries, err := Collect([]string{
		filepath.Join(dir, "loose.txt"),
		filepath.Join(dir, "tree"),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := names(entries); !equal(got, []string{"loose.txt", "tree/a.txt"}) {
		t.Errorf("names = %v, want [loose.txt tree/a.txt]", got)
	}
}

// "." must resolve to the current directory's real name, not to a literal dot.
func TestCollectRelativeCurrentDirectory(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "inside.txt"), "x")
	t.Chdir(dir)

	entries, err := Collect([]string{"."})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	prefix := filepath.Base(dir) + "/"
	if !strings.HasPrefix(entries[0].Name, prefix) {
		t.Errorf("name = %q, want it to start with %q", entries[0].Name, prefix)
	}
}

// Following symlinks risks cycles and pulls in data from outside the selected
// tree, so they are skipped.
func TestCollectSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "outside.txt"), "secret")
	writeFile(t, filepath.Join(dir, "tree", "a.txt"), "a")
	if err := os.Symlink(filepath.Join(dir, "outside.txt"), filepath.Join(dir, "tree", "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	entries, err := Collect([]string{filepath.Join(dir, "tree")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := names(entries); !equal(got, []string{"tree/a.txt"}) {
		t.Errorf("names = %v, want [tree/a.txt] — the symlink must be skipped", got)
	}
}

func TestCollectEmptyDirectoryIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Collect([]string{filepath.Join(dir, "empty")}); err == nil {
		t.Error("collecting an empty directory should report that there is nothing to send")
	}
}

func TestCollectMissingPath(t *testing.T) {
	if _, err := Collect([]string{filepath.Join(t.TempDir(), "nope.txt")}); err == nil {
		t.Error("a missing path must fail before any network access")
	}
}

func TestTotalSize(t *testing.T) {
	entries := []Entry{{Size: 10}, {Size: 32}, {Size: 0}}
	if got := TotalSize(entries); got != 42 {
		t.Errorf("TotalSize = %d, want 42", got)
	}
	if got := TotalSize(nil); got != 0 {
		t.Errorf("TotalSize(nil) = %d, want 0", got)
	}
}

func names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	sort.Strings(out)
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			return false
		}
	}
	return true
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
