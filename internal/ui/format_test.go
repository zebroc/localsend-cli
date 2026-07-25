package ui

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	tests := map[int64]string{
		0:          "0 B",
		1:          "1 B",
		1023:       "1023 B",
		1024:       "1.0 KiB",
		1536:       "1.5 KiB",
		1048576:    "1.0 MiB",
		41943040:   "40.0 MiB",
		1073741824: "1.0 GiB",
	}
	for in, want := range tests {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// Transfers on a fast link finish in well under a second; rounding those to
// "0s" would make the log look broken.
func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{530 * time.Microsecond, "530µs"},
		{2 * time.Millisecond, "2ms"},
		{218 * time.Millisecond, "218ms"},
		{592 * time.Millisecond, "592ms"},
		{1500 * time.Millisecond, "1.5s"},
		{65 * time.Second, "1m5s"},
	}
	for _, test := range tests {
		if got := FormatDuration(test.in); got != test.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", test.in, got, test.want)
		}
	}
	if got := FormatDuration(400 * time.Microsecond); got == "0s" {
		t.Error("a sub-millisecond duration must not be rendered as 0s")
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "file", "files"); got != "1 file" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(4, "file", "files"); got != "4 files" {
		t.Errorf("plural(4) = %q", got)
	}
	if got := plural(0, "file", "files"); got != "0 files" {
		t.Errorf("plural(0) = %q", got)
	}
}

// The progress reader must pass the data through untouched while counting it.
func TestProgressReaderCounts(t *testing.T) {
	progress := NewProgress(1, 11)
	progress.StartFile("a.txt", 11)

	got, err := io.ReadAll(progress.Reader(strings.NewReader("hello world")))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("read %q, want the data unchanged", got)
	}
	if progress.fileDone != 11 {
		t.Errorf("counted %d bytes, want 11", progress.fileDone)
	}
}
