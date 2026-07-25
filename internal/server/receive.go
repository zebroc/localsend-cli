package server

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zebroc/localsend-cli/internal/proto"
)

// maxSegment keeps every path component below the 255 byte limit of common
// file systems, with room for the ".part" suffix used while writing.
const maxSegment = 200

// maxCollisions bounds the search for a free file name.
const maxCollisions = 1000

// Receiver writes incoming files below Dir.
type Receiver struct {
	Dir string
}

// Save streams body into the file described by f and returns the path written.
//
// The file is written to "<target>.part" first and renamed once the expected
// number of bytes arrived, so a partial transfer never looks complete.
func (r *Receiver) Save(f proto.File, body io.Reader) (string, error) {
	rel, err := sanitizeRelPath(f.FileName)
	if err != nil {
		return "", err
	}

	full := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if !withinDir(r.Dir, full) {
		return "", fmt.Errorf("refusing path outside of destination: %q", f.FileName)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}

	file, target, err := createTarget(full)
	if err != nil {
		return "", err
	}
	tempPath := target + ".part"

	// Read one byte more than announced so a sender that lies about the size
	// fails instead of silently producing a truncated file.
	written, copyErr := io.Copy(file, io.LimitReader(body, f.Size+1))
	closeErr := file.Close()

	switch {
	case copyErr != nil:
		os.Remove(tempPath)
		return "", copyErr
	case closeErr != nil:
		os.Remove(tempPath)
		return "", closeErr
	case written != f.Size:
		os.Remove(tempPath)
		return "", fmt.Errorf("expected %d bytes, received %d", f.Size, written)
	}

	if err := os.Rename(tempPath, target); err != nil {
		os.Remove(tempPath)
		return "", err
	}

	if f.Metadata != nil && f.Metadata.Modified != "" {
		if modified, err := time.Parse(time.RFC3339, f.Metadata.Modified); err == nil {
			_ = os.Chtimes(target, time.Time{}, modified)
		}
	}
	return target, nil
}

// createTarget opens "<path>.part" for writing, appending " (n)" to the name
// until both the temporary and the final path are free.
//
// O_EXCL on the temporary file claims the name atomically, so two files of the
// same transfer cannot pick the same target.
func createTarget(full string) (*os.File, string, error) {
	for i := range maxCollisions {
		candidate := full
		if i > 0 {
			candidate = withCount(full, i)
		}
		file, err := os.OpenFile(candidate+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, "", err
		}
		if _, err := os.Lstat(candidate); err == nil {
			// Taken since the last check; release the claim and move on.
			file.Close()
			os.Remove(candidate + ".part")
			continue
		}
		return file, candidate, nil
	}
	return nil, "", fmt.Errorf("no free file name for %q", filepath.Base(full))
}

// countSuffix matches a trailing " (12)" so repeated transfers do not pile up
// as "file (1) (1).txt". Mirrors the official app's behaviour.
var countSuffix = regexp.MustCompile(`\s\(\d+\)$`)

// withCount inserts " (n)" before the extension: "a.txt" -> "a (1).txt".
func withCount(p string, n int) string {
	dir, name := filepath.Split(p)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	stem = countSuffix.ReplaceAllString(stem, "")
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
}

// sanitizeRelPath turns a fileName from the wire into a relative path that
// cannot escape the destination directory.
//
// Senders may include a directory prefix ("Photos/2024/img.jpg"); Windows
// senders use backslashes. Everything else — "..", absolute paths, drive
// letters, control characters — is dropped or rejected.
func sanitizeRelPath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")

	var segments []string
	for segment := range strings.SplitSeq(name, "/") {
		segment = strings.Map(dropControl, segment)
		segment = strings.TrimSpace(segment)

		switch segment {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("rejecting path traversal in %q", name)
		}
		if len(segments) == 0 && isDriveLetter(segment) {
			continue
		}
		if len(segment) > maxSegment {
			segment = truncateSegment(segment)
		}
		segments = append(segments, segment)
	}

	if len(segments) == 0 {
		return "unnamed", nil
	}
	return path.Join(segments...), nil
}

func dropControl(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return -1
	}
	return r
}

func isDriveLetter(segment string) bool {
	return len(segment) == 2 && segment[1] == ':' &&
		((segment[0] >= 'a' && segment[0] <= 'z') || (segment[0] >= 'A' && segment[0] <= 'Z'))
}

// truncateSegment shortens a name while keeping its extension.
func truncateSegment(segment string) string {
	ext := path.Ext(segment)
	if len(ext) > 32 {
		// Not really an extension; cut bluntly.
		return segment[:maxSegment]
	}
	return segment[:maxSegment-len(ext)] + ext
}

// withinDir reports whether target stays inside dir.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
