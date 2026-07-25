// Package send implements the sending side: collecting local paths and pushing
// them to a peer.
package send

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zebroc/localsend-cli/internal/ui"
)

// Entry is one file to transfer.
type Entry struct {
	// Path is the local file to read.
	Path string

	// Name is the fileName sent over the wire. For a directory it carries the
	// relative path below the directory, always with "/" separators.
	Name string

	Size     int64
	Modified time.Time
}

// Collect expands the given paths into the files to send.
//
// Files are taken by their base name; directories are walked recursively and
// contribute "<dir>/<relative path>" names, which is how LocalSend transfers a
// folder — the protocol has no folder concept of its own.
//
// Symlinks below a directory are skipped: following them risks cycles and
// sending data from outside the selected tree.
func Collect(paths []string) ([]Entry, error) {
	var entries []Entry

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}

		switch {
		case info.IsDir():
			collected, err := collectDir(p)
			if err != nil {
				return nil, err
			}
			if len(collected) == 0 {
				ui.Warnf("%s contains no files, skipping", p)
			}
			entries = append(entries, collected...)

		case info.Mode().IsRegular():
			entries = append(entries, Entry{
				Path:     p,
				Name:     filepath.Base(p),
				Size:     info.Size(),
				Modified: info.ModTime(),
			})

		default:
			ui.Warnf("%s is not a regular file, skipping", p)
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("nothing to send")
	}
	return entries, nil
}

func collectDir(dir string) ([]Entry, error) {
	root := filepath.Clean(dir)
	prefix := dirPrefix(root)

	var entries []Entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory should not abort the whole transfer.
			ui.Warnf("skipping %s: %v", path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			ui.Warnf("skipping symlink %s", path)
			return nil
		}
		if !d.Type().IsRegular() {
			ui.Warnf("skipping %s (not a regular file)", path)
			return nil
		}

		info, err := d.Info()
		if err != nil {
			ui.Warnf("skipping %s: %v", path, err)
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, Entry{
			Path:     path,
			Name:     prefix + "/" + filepath.ToSlash(rel),
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
		return nil
	})
	return entries, err
}

// dirPrefix is the folder name the receiver will create. Resolving to an
// absolute path first makes "." and trailing slashes behave as expected.
func dirPrefix(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) || strings.Trim(name, `\/`) == "" {
		return "files"
	}
	return name
}

// TotalSize sums the size of all entries.
func TotalSize(entries []Entry) int64 {
	var total int64
	for _, entry := range entries {
		total += entry.Size
	}
	return total
}
