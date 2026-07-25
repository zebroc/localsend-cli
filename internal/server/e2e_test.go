package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/send"
	"github.com/zebroc/localsend-cli/internal/server"
)

// TestSendFilesAndDirectory drives a real transfer over TLS through the whole
// stack: collect local paths, prepare-upload, upload, save. It is the closest
// we get to the app without the app.
func TestSendFilesAndDirectory(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()

	bigContent := bytes.Repeat([]byte("localsend"), 600_000) // ~5.4 MiB
	writeFile(t, filepath.Join(src, "single.txt"), []byte("top level file"))
	writeFile(t, filepath.Join(src, "tree", "a.txt"), []byte("inside a"))
	writeFile(t, filepath.Join(src, "tree", "sub", "big.bin"), bigContent)
	writeFile(t, filepath.Join(src, "tree", "Ümlaut Datei.txt"), []byte("äöü"))
	// An empty directory has no representation in the protocol and is expected
	// to disappear.
	if err := os.MkdirAll(filepath.Join(src, "tree", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	device := startReceiver(t, receiverID, dst, "")

	entries, err := send.Collect([]string{
		filepath.Join(src, "single.txt"),
		filepath.Join(src, "tree"),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("collected %d entries, want 4: %+v", len(entries), entries)
	}

	if err := send.Send(context.Background(), send.Options{
		Identity: senderID,
		Target:   device,
		Entries:  entries,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	assertFile(t, filepath.Join(dst, "single.txt"), []byte("top level file"))
	assertFile(t, filepath.Join(dst, "tree", "a.txt"), []byte("inside a"))
	assertFile(t, filepath.Join(dst, "tree", "Ümlaut Datei.txt"), []byte("äöü"))
	assertFile(t, filepath.Join(dst, "tree", "sub", "big.bin"), bigContent)

	if _, err := os.Stat(filepath.Join(dst, "tree", "empty")); err == nil {
		t.Error("an empty directory was recreated, which the protocol cannot express")
	}
}

// A second transfer of the same names must not overwrite anything.
func TestSendTwiceKeepsBothCopies(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "report.pdf"), []byte("first"))

	device := startReceiver(t, receiverID, dst, "")

	for _, content := range []string{"first", "second"} {
		writeFile(t, filepath.Join(src, "report.pdf"), []byte(content))
		entries, err := send.Collect([]string{filepath.Join(src, "report.pdf")})
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if err := send.Send(context.Background(), send.Options{
			Identity: senderID, Target: device, Entries: entries,
		}); err != nil {
			t.Fatalf("Send %q: %v", content, err)
		}
	}

	assertFile(t, filepath.Join(dst, "report.pdf"), []byte("first"))
	assertFile(t, filepath.Join(dst, "report (1).pdf"), []byte("second"))
}

// With --parallel the uploads run through a worker pool instead of one after
// another; every file must still arrive intact.
func TestSendInParallel(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()
	contents := map[string][]byte{
		"a.txt": bytes.Repeat([]byte("a"), 1024),
		"b.txt": bytes.Repeat([]byte("b"), 200_000),
		"c.txt": bytes.Repeat([]byte("c"), 3),
		"d.bin": bytes.Repeat([]byte{0x00, 0xff}, 50_000),
		"e.txt": []byte("last"),
	}
	paths := make([]string, 0, len(contents))
	for name, content := range contents {
		writeFile(t, filepath.Join(src, name), content)
		paths = append(paths, filepath.Join(src, name))
	}

	device := startReceiver(t, receiverID, dst, "")
	entries, err := send.Collect(paths)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := send.Send(context.Background(), send.Options{
		Identity: senderID, Target: device, Entries: entries, Parallel: 4,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for name, content := range contents {
		assertFile(t, filepath.Join(dst, name), content)
	}
}

// When one file cannot be read the transfer must fail loudly and tell the
// receiver to drop the session, rather than leaving it half-finished.
func TestFailedUploadIsReported(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "present.txt"), []byte("here"))
	writeFile(t, filepath.Join(src, "vanishing.txt"), []byte("gone before the upload"))

	device := startReceiver(t, receiverID, dst, "")
	entries, err := send.Collect([]string{
		filepath.Join(src, "present.txt"),
		filepath.Join(src, "vanishing.txt"),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// The metadata is already collected; removing the file now makes the upload
	// itself fail, which is what happens when a file disappears mid-transfer.
	if err := os.Remove(filepath.Join(src, "vanishing.txt")); err != nil {
		t.Fatal(err)
	}

	err = send.Send(context.Background(), send.Options{
		Identity: senderID, Target: device, Entries: entries,
	})
	if err == nil {
		t.Fatal("Send should fail when a file cannot be read")
	}
	if !strings.Contains(err.Error(), "vanishing.txt") {
		t.Errorf("error %q should name the file that failed", err)
	}

	// The file that made it through is kept; the failed one leaves nothing
	// behind, not even a .part file.
	assertFile(t, filepath.Join(dst, "present.txt"), []byte("here"))
	for _, name := range []string{"vanishing.txt", "vanishing.txt.part"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s should not exist", name)
		}
	}
}

func TestPinIsEnforced(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "secret.txt"), []byte("classified"))

	device := startReceiver(t, receiverID, dst, "123456")

	entries, err := send.Collect([]string{filepath.Join(src, "secret.txt")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	err = send.Send(context.Background(), send.Options{
		Identity: senderID, Target: device, Entries: entries, PIN: "000000",
	})
	if err == nil {
		t.Fatal("a transfer with the wrong PIN must fail")
	}
	if !strings.Contains(err.Error(), "PIN") {
		t.Errorf("error %q should mention the PIN", err)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("the destination should be empty, found %d entries", len(entries))
	}

	if err := send.Send(context.Background(), send.Options{
		Identity: senderID, Target: device, Entries: entries, PIN: "123456",
	}); err != nil {
		t.Fatalf("Send with the correct PIN: %v", err)
	}
	assertFile(t, filepath.Join(dst, "secret.txt"), []byte("classified"))
}

// The short-lived server used during discovery has no destination directory and
// must refuse transfers instead of dropping files somewhere.
func TestServerWithoutDestinationRejects(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("x"))

	device := startReceiver(t, receiverID, "", "")

	entries, err := send.Collect([]string{filepath.Join(src, "a.txt")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	err = send.Send(context.Background(), send.Options{
		Identity: senderID, Target: device, Entries: entries,
	})
	if err == nil {
		t.Fatal("a discovery-only server must reject transfers")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q should say the transfer was rejected", err)
	}
}

func TestSendOverPlainHTTP(t *testing.T) {
	receiverID := loadIdentity(t, "Receiver")
	senderID := loadIdentity(t, "Sender")

	dst := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "plain.txt"), []byte("no tls"))

	srv, err := server.New(server.Config{Identity: receiverID, Port: 0, UseTLS: false, DstDir: dst})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	serveUntilCleanup(t, srv)

	entries, err := send.Collect([]string{filepath.Join(src, "plain.txt")})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := send.Send(context.Background(), send.Options{
		Identity: senderID,
		Target:   proto.Device{Alias: "Receiver", IP: "127.0.0.1", Port: srv.Port(), Protocol: proto.ProtocolHTTP},
		Entries:  entries,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	assertFile(t, filepath.Join(dst, "plain.txt"), []byte("no tls"))
}

// startReceiver runs a TLS server and returns the device pointing at it.
func startReceiver(t *testing.T, id *identity.Identity, dst, pin string) proto.Device {
	t.Helper()

	srv, err := server.New(server.Config{
		Identity: id,
		Port:     0,
		UseTLS:   true,
		DstDir:   dst,
		PIN:      pin,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	serveUntilCleanup(t, srv)

	return proto.Device{
		Alias:       id.Alias,
		IP:          "127.0.0.1",
		Port:        srv.Port(),
		Protocol:    proto.ProtocolHTTPS,
		Fingerprint: id.Fingerprint,
	}
}

func serveUntilCleanup(t *testing.T, srv *server.Server) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// loadIdentity creates a fresh identity in its own configuration directory.
func loadIdentity(t *testing.T, alias string) *identity.Identity {
	t.Helper()

	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	id, err := identity.Load(alias)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}
	return id
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if !bytes.Equal(got, want) {
		if len(want) > 256 {
			t.Errorf("%s: content differs (sha256 %x, want %x, %d vs %d bytes)",
				path, sha256.Sum256(got), sha256.Sum256(want), len(got), len(want))
			return
		}
		t.Errorf("%s: content = %q, want %q", path, got, want)
	}
}
