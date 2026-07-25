package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// lineActive tracks whether an unterminated progress line is on stdout, so log
// output can wipe it before writing. Guarded by mu.
var lineActive bool

func clearLineLocked() {
	if lineActive {
		fmt.Fprint(os.Stdout, "\r\x1b[K")
		lineActive = false
	}
}

// IsTerminal reports whether stdout is a character device, i.e. whether we may
// use carriage returns to redraw a progress line.
func IsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Progress renders the progress of a multi-file transfer.
//
// On a terminal it redraws a single line; otherwise it prints one line per
// finished file so logs stay readable.
type Progress struct {
	tty        bool
	totalFiles int
	totalBytes int64

	fileIndex  int
	fileName   string
	fileSize   int64
	fileDone   int64
	bytesDone  int64
	start      time.Time
	lastRender time.Time
}

// NewProgress creates a progress renderer for a transfer of the given size.
func NewProgress(totalFiles int, totalBytes int64) *Progress {
	return &Progress{
		tty:        IsTerminal(),
		totalFiles: totalFiles,
		totalBytes: totalBytes,
		start:      time.Now(),
	}
}

// StartFile announces the file that is being transferred now.
func (p *Progress) StartFile(name string, size int64) {
	p.fileIndex++
	p.fileName = name
	p.fileSize = size
	p.fileDone = 0
	p.render(true)
}

// Reader wraps r so that everything read from it counts towards the progress.
func (p *Progress) Reader(r io.Reader) io.Reader {
	return &progressReader{p: p, r: r}
}

// FinishFile closes the progress line for the current file.
//
// A failure is not reported here: the caller returns the error and it would
// otherwise be printed twice.
func (p *Progress) FinishFile(err error) {
	mu.Lock()
	clearLineLocked()
	mu.Unlock()

	if err != nil {
		return
	}
	if p.tty {
		Infof("  %s  %s", p.fileName, FormatBytes(p.fileSize))
	} else {
		Infof("sent %s (%s)", p.fileName, FormatBytes(p.fileSize))
	}
}

// Done prints the closing summary.
func (p *Progress) Done(files int, bytes int64) {
	elapsed := time.Since(p.start)
	rate := ""
	if secs := elapsed.Seconds(); secs > 0.05 && bytes > 0 {
		rate = fmt.Sprintf(" at %s/s", FormatBytes(int64(float64(bytes)/secs)))
	}
	Infof("%s in %s%s", plural(files, "file", "files"), FormatDuration(elapsed), rate)
}

// FormatDuration renders a duration without collapsing short transfers to "0s".
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return d.Round(10 * time.Microsecond).String()
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(100 * time.Millisecond).String()
	}
}

func (p *Progress) add(n int64) {
	p.fileDone += n
	p.bytesDone += n
	p.render(false)
}

func (p *Progress) render(force bool) {
	if !p.tty {
		return
	}
	if !force && time.Since(p.lastRender) < 80*time.Millisecond {
		return
	}
	p.lastRender = time.Now()

	pct := 0.0
	if p.totalBytes > 0 {
		pct = float64(p.bytesDone) / float64(p.totalBytes) * 100
	}
	name := p.fileName
	if len(name) > 40 {
		name = "…" + name[len(name)-39:]
	}

	mu.Lock()
	defer mu.Unlock()
	clearLineLocked()
	fmt.Fprintf(os.Stdout, "[%d/%d] %-40s %s / %s  %5.1f%%",
		p.fileIndex, p.totalFiles, name,
		FormatBytes(p.fileDone), FormatBytes(p.fileSize), pct)
	lineActive = true
}

type progressReader struct {
	p *Progress
	r io.Reader
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.p.add(int64(n))
	}
	return n, err
}

// FormatBytes renders a byte count in units a human can read at a glance.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// Table prints rows aligned in columns.
func Table(header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	render := func(cells []string) {
		parts := make([]string, 0, len(cells))
		for i, cell := range cells {
			if i == len(cells)-1 {
				parts = append(parts, cell)
			} else {
				parts = append(parts, fmt.Sprintf("%-*s", widths[i], cell))
			}
		}
		Infof("%s", strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	render(header)
	for _, row := range rows {
		render(row)
	}
}
