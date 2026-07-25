package peers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
)

func device(alias, ip, fingerprint string) proto.Device {
	return proto.Device{
		Alias:       alias,
		IP:          ip,
		Port:        proto.DefaultPort,
		Protocol:    proto.ProtocolHTTPS,
		Fingerprint: fingerprint,
	}
}

func TestPutAndLookup(t *testing.T) {
	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	cache := Load()

	phone := device("Pixel", "192.168.1.20", "AAAA")
	cache.Put(phone)

	found := cache.Lookup("pixel")
	if len(found) != 1 {
		t.Fatalf("Lookup found %d devices, want 1", len(found))
	}
	if found[0] != phone {
		t.Errorf("Lookup returned %+v, want %+v", found[0], phone)
	}
	if len(cache.Lookup("nothing-like-this")) != 0 {
		t.Error("Lookup should not match an unrelated alias")
	}
}

// An exact alias must win over a device that merely starts with the same
// letters, otherwise "Pixel" could resolve to "Pixel Tablet".
func TestLookupPrefersExactMatch(t *testing.T) {
	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	cache := Load()

	cache.Put(device("Pixel Tablet", "192.168.1.21", "BBBB"))
	cache.Put(device("Pixel", "192.168.1.20", "AAAA"))

	found := cache.Lookup("Pixel")
	if len(found) != 2 {
		t.Fatalf("Lookup found %d devices, want 2", len(found))
	}
	if found[0].Alias != "Pixel" {
		t.Errorf("first match is %q, want the exact match %q", found[0].Alias, "Pixel")
	}
}

func TestPutIgnoresIncompleteDevices(t *testing.T) {
	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	cache := Load()

	cache.Put(proto.Device{Alias: "", IP: "192.168.1.20"})
	cache.Put(proto.Device{Alias: "NoAddress", IP: ""})

	if all := cache.All(); len(all) != 0 {
		t.Errorf("cache holds %d devices, want 0: %+v", len(all), all)
	}
}

// The fingerprint identifies a device, so a changed address must update the
// existing entry instead of adding a second one.
func TestPutReplacesEntryWithSameFingerprint(t *testing.T) {
	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	cache := Load()

	cache.Put(device("Pixel", "192.168.1.20", "AAAA"))
	cache.Put(device("Pixel", "10.0.0.5", "AAAA"))

	all := cache.All()
	if len(all) != 1 {
		t.Fatalf("cache holds %d devices, want 1: %+v", len(all), all)
	}
	if all[0].IP != "10.0.0.5" {
		t.Errorf("ip = %q, want the newer address 10.0.0.5", all[0].IP)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(identity.ConfigDirEnv, dir)

	cache := Load()
	phone := device("Pixel", "192.168.1.20", "AAAA")
	cache.Put(phone)
	if err := cache.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("%s was not written: %v", fileName, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600", fileName, perm)
	}

	reloaded := Load()
	found := reloaded.Lookup("Pixel")
	if len(found) != 1 {
		t.Fatalf("after reload Lookup found %d devices, want 1", len(found))
	}
	if found[0] != phone {
		t.Errorf("reloaded %+v, want %+v", found[0], phone)
	}
}

// Save rewrites the file only when something changed, so a daemon seeing the
// same announcement every few minutes does not keep writing to disk.
func TestSaveSkipsUnchangedCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(identity.ConfigDirEnv, dir)

	cache := Load()
	cache.Put(device("Pixel", "192.168.1.20", "AAAA"))
	if err := cache.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(dir, fileName)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Re-seeing the same device only refreshes the timestamp in memory.
	cache.Put(device("Pixel", "192.168.1.20", "AAAA"))
	if err := cache.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("Save rewrote the file although nothing changed")
	}
}

func TestLoadDropsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(identity.ConfigDirEnv, dir)

	stored := []Entry{
		{Device: device("Fresh", "192.168.1.20", "AAAA"), LastSeen: time.Now().Add(-time.Hour)},
		{Device: device("Ancient", "192.168.1.21", "BBBB"), LastSeen: time.Now().Add(-maxAge - time.Hour)},
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cache := Load()
	all := cache.All()
	if len(all) != 1 {
		t.Fatalf("cache holds %d devices, want only the fresh one: %+v", len(all), all)
	}
	if all[0].Alias != "Fresh" {
		t.Errorf("kept %q, want %q", all[0].Alias, "Fresh")
	}
}

// The cache is an optimisation: a damaged or missing file must never stop a
// transfer.
func TestLoadToleratesBrokenFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(identity.ConfigDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cache := Load()
	if all := cache.All(); len(all) != 0 {
		t.Errorf("cache holds %d devices, want 0", len(all))
	}
	cache.Put(device("Pixel", "192.168.1.20", "AAAA"))
	if err := cache.Save(); err != nil {
		t.Errorf("Save after a broken file: %v", err)
	}
}
