package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zebroc/localsend-cli/internal/proto"
)

func TestSanitizeRelPath(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain file", in: "notes.txt", want: "notes.txt"},
		{name: "directory", in: "Photos/2024/img.jpg", want: "Photos/2024/img.jpg"},
		{name: "windows separators", in: `Photos\2024\img.jpg`, want: "Photos/2024/img.jpg"},
		{name: "drive letter", in: `C:\Users\me\a.txt`, want: "Users/me/a.txt"},
		{name: "absolute path", in: "/etc/passwd", want: "etc/passwd"},
		{name: "current dir segments", in: "a/./b/c.txt", want: "a/b/c.txt"},
		{name: "double slash", in: "a//b.txt", want: "a/b.txt"},
		{name: "empty", in: "", want: "unnamed"},
		{name: "only slashes", in: "///", want: "unnamed"},
		{name: "control characters", in: "a\x00b\x1fc.txt", want: "abc.txt"},
		{name: "spaces kept inside", in: "my report final.pdf", want: "my report final.pdf"},
		{name: "traversal", in: "../../etc/passwd", wantErr: true},
		{name: "traversal in the middle", in: "a/../../b.txt", wantErr: true},
		{name: "windows traversal", in: `..\..\secret`, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sanitizeRelPath(test.in)
			if test.wantErr {
				if err == nil {
					t.Fatalf("sanitizeRelPath(%q) = %q, want an error", test.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeRelPath(%q): %v", test.in, err)
			}
			if got != test.want {
				t.Errorf("sanitizeRelPath(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// Whatever a sender puts into fileName, the result must stay inside the
// destination directory.
func TestSanitizeRelPathStaysInsideDestination(t *testing.T) {
	dst := "/tmp/downloads"
	inputs := []string{
		"a.txt", "a/b/c.txt", "/etc/passwd", `C:\Windows\system32\evil.dll`,
		"....//a.txt", "a/./../b.txt", "\x00", "  spaced  /x.txt",
	}
	for _, in := range inputs {
		rel, err := sanitizeRelPath(in)
		if err != nil {
			continue // rejected outright, which is also fine
		}
		full := filepath.Join(dst, filepath.FromSlash(rel))
		if !withinDir(dst, full) {
			t.Errorf("sanitizeRelPath(%q) = %q escapes to %q", in, rel, full)
		}
	}
}

func TestSanitizeRelPathTruncatesLongSegments(t *testing.T) {
	long := strings.Repeat("x", 400) + ".txt"
	got, err := sanitizeRelPath(long)
	if err != nil {
		t.Fatalf("sanitizeRelPath: %v", err)
	}
	if len(got) > maxSegment {
		t.Errorf("segment length %d exceeds %d", len(got), maxSegment)
	}
	if !strings.HasSuffix(got, ".txt") {
		t.Errorf("truncated name %q lost its extension", got)
	}
}

func TestWithCount(t *testing.T) {
	tests := []struct {
		in    string
		count int
		want  string
	}{
		{"/tmp/a.txt", 1, "/tmp/a (1).txt"},
		{"/tmp/a (1).txt", 2, "/tmp/a (2).txt"},
		{"/tmp/a (12).txt", 3, "/tmp/a (3).txt"},
		{"/tmp/noext", 1, "/tmp/noext (1)"},
		{"/tmp/archive.tar.gz", 1, "/tmp/archive.tar (1).gz"},
		{"/tmp/dir/A.TXT", 1, "/tmp/dir/A (1).TXT"},
	}
	for _, test := range tests {
		if got := withCount(test.in, test.count); got != test.want {
			t.Errorf("withCount(%q, %d) = %q, want %q", test.in, test.count, got, test.want)
		}
	}
}

func TestSaveWritesFileAndCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	content := "hello localsend"
	path, err := receiver.Save(
		proto.File{ID: "0", FileName: "Photos/2024/note.txt", Size: int64(len(content))},
		strings.NewReader(content),
	)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	want := filepath.Join(dir, "Photos", "2024", "note.txt")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestSaveRenamesOnCollision(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	for i, want := range []string{"a.txt", "a (1).txt", "a (2).txt"} {
		content := strings.Repeat("x", i+1)
		path, err := receiver.Save(
			proto.File{ID: "0", FileName: "a.txt", Size: int64(len(content))},
			strings.NewReader(content),
		)
		if err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		if filepath.Base(path) != want {
			t.Errorf("save %d landed in %q, want base %q", i, path, want)
		}
	}
}

// A sender that announces more bytes than it delivers must not leave a
// truncated file behind that looks complete.
func TestSaveRejectsShortBody(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	_, err := receiver.Save(
		proto.File{ID: "0", FileName: "short.bin", Size: 100},
		strings.NewReader("only a few bytes"),
	)
	if err == nil {
		t.Fatal("Save should fail when fewer bytes arrive than announced")
	}
	assertDirEmpty(t, dir)
}

func TestSaveRejectsOversizedBody(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	_, err := receiver.Save(
		proto.File{ID: "0", FileName: "long.bin", Size: 4},
		strings.NewReader("far more than four bytes"),
	)
	if err == nil {
		t.Fatal("Save should fail when more bytes arrive than announced")
	}
	assertDirEmpty(t, dir)
}

func TestSaveRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	if _, err := receiver.Save(
		proto.File{ID: "0", FileName: "../escaped.txt", Size: 1},
		strings.NewReader("x"),
	); err == nil {
		t.Fatal("Save should reject a path traversal")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
		t.Fatal("a file was written outside the destination directory")
	}
}

func TestSaveAppliesModifiedTime(t *testing.T) {
	dir := t.TempDir()
	receiver := &Receiver{Dir: dir}

	path, err := receiver.Save(
		proto.File{
			ID:       "0",
			FileName: "stamped.txt",
			Size:     1,
			Metadata: &proto.FileMetadata{Modified: "2021-01-01T12:34:56Z"},
		},
		strings.NewReader("x"),
	)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.ModTime().UTC().Format("2006-01-02T15:04:05Z"); got != "2021-01-01T12:34:56Z" {
		t.Errorf("modification time = %s, want 2021-01-01T12:34:56Z", got)
	}
}

// assertDirEmpty fails when dir contains anything, including leftover .part
// files.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("%s should be empty, contains: %s", dir, strings.Join(names, ", "))
	}
}
