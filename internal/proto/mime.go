package proto

import (
	"mime"
	"path"
	"strings"
)

// FileTypeOf guesses the MIME type from a file name, which is what the v2
// protocol puts into File.FileType.
//
// Parameters are stripped ("text/plain; charset=utf-8" -> "text/plain") because
// the reference implementation sends a bare MIME type.
func FileTypeOf(fileName string) string {
	ext := path.Ext(fileName)
	if ext == "" {
		return "application/octet-stream"
	}
	t := mime.TypeByExtension(ext)
	if t == "" {
		if extra, ok := extraTypes[strings.ToLower(ext)]; ok {
			return extra
		}
		return "application/octet-stream"
	}
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// extraTypes covers extensions the Go standard library does not know but that
// are common in LocalSend transfers.
var extraTypes = map[string]string{
	".apk":  "application/vnd.android.package-archive",
	".dng":  "image/x-adobe-dng",
	".heic": "image/heic",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".opus": "audio/opus",
}
