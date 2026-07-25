package send

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/zebroc/localsend-cli/internal/httpx"
	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// Options configures a transfer.
type Options struct {
	Identity *identity.Identity
	Target   proto.Device
	Entries  []Entry

	// PIN is appended to prepare-upload when the receiver asks for one.
	PIN string

	// Parallel is the number of concurrent uploads. Values below 2 upload one
	// file after another, which keeps the progress display readable.
	Parallel int

	// Port is announced to the receiver so it can send something back. The
	// default port is right whenever a daemon runs on this host, which is the
	// common setup.
	Port int
}

// Send transfers all entries to the target device.
func Send(ctx context.Context, opts Options) error {
	if len(opts.Entries) == 0 {
		return fmt.Errorf("nothing to send")
	}

	client := httpx.New(httpx.Options{Identity: opts.Identity})
	base := opts.Target.BaseURL()

	files := make(map[string]proto.File, len(opts.Entries))
	byID := make(map[string]Entry, len(opts.Entries))
	for i, entry := range opts.Entries {
		id := strconv.Itoa(i)
		files[id] = proto.File{
			ID:       id,
			FileName: entry.Name,
			Size:     entry.Size,
			FileType: proto.FileTypeOf(entry.Name),
			Metadata: &proto.FileMetadata{Modified: entry.Modified.UTC().Format(time.RFC3339)},
		}
		byID[id] = entry
	}

	port := opts.Port
	if port == 0 {
		port = proto.DefaultPort
	}
	request := proto.PrepareUploadRequest{
		Info: proto.Register{
			Alias:       opts.Identity.Alias,
			Version:     proto.Version,
			DeviceType:  proto.DeviceTypeHeadless,
			Fingerprint: opts.Identity.Fingerprint,
			Port:        port,
			Protocol:    proto.ProtocolHTTPS,
		},
		Files: files,
	}

	prepareURL := base + "/prepare-upload"
	if opts.PIN != "" {
		prepareURL += "?pin=" + url.QueryEscape(opts.PIN)
	}

	total := TotalSize(opts.Entries)
	ui.Infof("sending %d file(s), %s to %s", len(opts.Entries), ui.FormatBytes(total), opts.Target)

	var prepared proto.PrepareUploadResponse
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, 60*time.Second)
	err := httpx.PostJSON(prepareCtx, client, prepareURL, request, &prepared)
	cancelPrepare()
	if err != nil {
		return describePrepareError(err)
	}
	if prepared.SessionID == "" || len(prepared.Files) == 0 {
		return fmt.Errorf("%s accepted none of the files", opts.Target.Alias)
	}

	// The receiver may accept only a subset.
	for id, entry := range byID {
		if _, ok := prepared.Files[id]; !ok {
			ui.Warnf("%s was rejected by the receiver", entry.Name)
		}
	}

	uploader := &uploader{
		client:    client,
		base:      base,
		sessionID: prepared.SessionID,
		progress:  ui.NewProgress(len(prepared.Files), acceptedSize(prepared.Files, byID)),
		quiet:     opts.Parallel > 1,
	}

	err = uploader.run(ctx, opts, prepared, byID)
	if err != nil {
		// Tell the receiver to drop the session so it does not wait for the
		// remaining files.
		uploader.cancelSession()
		return err
	}
	uploader.progress.Done(len(prepared.Files), uploader.sent)
	return nil
}

type uploader struct {
	client    *http.Client
	base      string
	sessionID string
	progress  *ui.Progress
	quiet     bool

	mu   sync.Mutex
	sent int64
}

func (u *uploader) run(ctx context.Context, opts Options, prepared proto.PrepareUploadResponse, byID map[string]Entry) error {
	// Preserve the order the user gave us; map iteration would shuffle it.
	type job struct {
		id    string
		token string
		entry Entry
	}
	var jobs []job
	for i := range opts.Entries {
		id := strconv.Itoa(i)
		if token, ok := prepared.Files[id]; ok {
			jobs = append(jobs, job{id: id, token: token, entry: byID[id]})
		}
	}

	workers := min(max(opts.Parallel, 1), len(jobs))

	if workers == 1 {
		for _, j := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			u.progress.StartFile(j.entry.Name, j.entry.Size)
			err := u.uploadFile(ctx, j.id, j.token, j.entry)
			u.progress.FinishFile(err)
			if err != nil {
				return fmt.Errorf("%s: %w", j.entry.Name, err)
			}
		}
		return nil
	}

	queue := make(chan job)
	var wg sync.WaitGroup
	errs := make(chan error, len(jobs))
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for range workers {
		wg.Go(func() {
			for j := range queue {
				err := u.uploadFile(uploadCtx, j.id, j.token, j.entry)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", j.entry.Name, err)
					cancel()
					return
				}
				ui.Infof("sent %s (%s)", j.entry.Name, ui.FormatBytes(j.entry.Size))
			}
		})
	}

	for _, j := range jobs {
		select {
		case queue <- j:
		case <-uploadCtx.Done():
		}
	}
	close(queue)
	wg.Wait()

	select {
	case err := <-errs:
		return err
	default:
		return ctx.Err()
	}
}

func (u *uploader) uploadFile(ctx context.Context, fileID, token string, entry Entry) error {
	file, err := os.Open(entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()

	target := fmt.Sprintf("%s/upload?sessionId=%s&fileId=%s&token=%s",
		u.base, url.QueryEscape(u.sessionID), url.QueryEscape(fileID), url.QueryEscape(token))

	var body io.Reader = u.countingReader(file)
	if !u.quiet {
		body = u.progress.Reader(body)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return err
	}
	// Without an explicit length Go would use chunked encoding, which some
	// receivers handle poorly.
	req.ContentLength = entry.Size
	req.Header.Set("Content-Type", "application/octet-stream")

	res, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return httpx.StatusErrorFrom(res)
	}
	return nil
}

// cancelSession asks the receiver to forget the session. Best effort: the
// transfer already failed when this runs.
func (u *uploader) cancelSession() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target := u.base + "/cancel?sessionId=" + url.QueryEscape(u.sessionID)
	if err := httpx.PostJSON(ctx, u.client, target, nil, nil); err != nil {
		ui.Debugf("cancelling session: %v", err)
	}
}

func (u *uploader) countingReader(file *os.File) *countingReader {
	return &countingReader{file: file, uploader: u}
}

type countingReader struct {
	file     *os.File
	uploader *uploader
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.file.Read(b)
	if n > 0 {
		c.uploader.mu.Lock()
		c.uploader.sent += int64(n)
		c.uploader.mu.Unlock()
	}
	return n, err
}

func acceptedSize(accepted map[string]string, byID map[string]Entry) int64 {
	var total int64
	for id := range accepted {
		total += byID[id].Size
	}
	return total
}

// describePrepareError turns the protocol's status codes into advice.
func describePrepareError(err error) error {
	switch httpx.StatusCode(err) {
	case http.StatusUnauthorized:
		return errors.New("the receiver requires a PIN — pass it with --pin")
	case http.StatusForbidden:
		return errors.New("the receiver rejected the transfer")
	case http.StatusConflict:
		return errors.New("the receiver is busy with another transfer")
	case http.StatusTooManyRequests:
		return errors.New("too many attempts — the receiver is rate-limiting us")
	default:
		return err
	}
}
