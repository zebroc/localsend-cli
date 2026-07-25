package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// The payloads below are taken verbatim from the protocol specification
// (~/Downloads/protocol/README.md), with the JSON5 comments removed.

func TestUnmarshalAnnouncement(t *testing.T) {
	const payload = `{
	  "alias": "Nice Orange",
	  "version": "2.0",
	  "deviceModel": "Samsung",
	  "deviceType": "mobile",
	  "fingerprint": "random string",
	  "port": 53317,
	  "protocol": "https",
	  "download": true,
	  "announce": true
	}`

	var message Multicast
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if message.Alias != "Nice Orange" || message.Port != 53317 || message.Protocol != "https" {
		t.Errorf("unexpected message: %+v", message)
	}
	if !message.Download {
		t.Error("download should be true")
	}
	if !message.IsAnnouncement() {
		t.Error("announce: true must count as an announcement")
	}
}

// v1 peers use "announcement" instead of "announce"; both must be understood
// and both must be written.
func TestAnnouncementLegacyKey(t *testing.T) {
	var message Multicast
	if err := json.Unmarshal([]byte(`{"alias":"Old","announcement":true}`), &message); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !message.IsAnnouncement() {
		t.Error("announcement: true must count as an announcement")
	}

	encoded, err := json.Marshal(Multicast{Alias: "Us", Announce: true, Announcement: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"announce":true`, `"announcement":true`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("encoded announcement %s is missing %s", encoded, key)
		}
	}
}

func TestUnmarshalRegisterResponseWithoutDownload(t *testing.T) {
	var response RegisterResponse
	if err := json.Unmarshal([]byte(`{"alias":"Test","version":"2.0","fingerprint":"abc"}`), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if response.Download {
		t.Error("download must default to false when absent")
	}
	if response.Alias != "Test" {
		t.Errorf("alias = %q", response.Alias)
	}
}

func TestUnmarshalPrepareUpload(t *testing.T) {
	const payload = `{
	  "info": {
	    "alias": "Nice Orange",
	    "version": "2.0",
	    "deviceModel": "Samsung",
	    "deviceType": "mobile",
	    "fingerprint": "random string",
	    "port": 53317,
	    "protocol": "https",
	    "download": true
	  },
	  "files": {
	    "some file id": {
	      "id": "some file id",
	      "fileName": "my image.png",
	      "size": 324242,
	      "fileType": "image/jpeg",
	      "sha256": "hash",
	      "preview": "data",
	      "metadata": {
	        "modified": "2021-01-01T12:34:56Z",
	        "accessed": "2021-01-01T12:34:56Z"
	      }
	    },
	    "another file id": {
	      "id": "another file id",
	      "fileName": "folder/another image.jpg",
	      "size": 1234,
	      "fileType": "image"
	    }
	  }
	}`

	var request PrepareUploadRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(request.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(request.Files))
	}

	first := request.Files["some file id"]
	if first.Size != 324242 || first.FileName != "my image.png" {
		t.Errorf("unexpected file: %+v", first)
	}
	if first.Metadata == nil || first.Metadata.Modified != "2021-01-01T12:34:56Z" {
		t.Errorf("metadata not parsed: %+v", first.Metadata)
	}

	// The legacy v1 enum must survive too — we only pass fileType through.
	second := request.Files["another file id"]
	if second.FileType != "image" {
		t.Errorf("fileType = %q, want %q", second.FileType, "image")
	}
	if second.Metadata != nil {
		t.Errorf("metadata should be nil, got %+v", second.Metadata)
	}
}

// Optional fields must be omitted rather than sent as null: some receivers are
// strict about types.
func TestMarshalFileOmitsEmptyOptionals(t *testing.T) {
	encoded, err := json.Marshal(File{ID: "0", FileName: "a.txt", Size: 3, FileType: "text/plain"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"sha256", "preview", "metadata"} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("encoded file %s should not contain %q", encoded, key)
		}
	}
}

func TestFileTypeOf(t *testing.T) {
	tests := map[string]string{
		"photo.jpg":         "image/jpeg",
		"photo.JPG":         "image/jpeg",
		"notes.txt":         "text/plain",
		"app.apk":           "application/vnd.android.package-archive",
		"clip.mov":          "video/quicktime",
		"archive.zip":       "application/zip",
		"no-extension":      "application/octet-stream",
		"weird.unknownext":  "application/octet-stream",
		"folder/inside.pdf": "application/pdf",
	}
	for name, want := range tests {
		if got := FileTypeOf(name); got != want {
			t.Errorf("FileTypeOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// Parameters like "; charset=utf-8" must not leak into the wire format.
func TestFileTypeHasNoParameters(t *testing.T) {
	if got := FileTypeOf("notes.txt"); strings.Contains(got, ";") {
		t.Errorf("FileTypeOf(notes.txt) = %q, want no parameters", got)
	}
}

func TestDeviceBaseURL(t *testing.T) {
	tests := []struct {
		device Device
		want   string
	}{
		{Device{IP: "192.168.1.5", Port: 53317, Protocol: "https"}, "https://192.168.1.5:53317/api/localsend/v2"},
		{Device{IP: "192.168.1.5", Port: 8080, Protocol: "http"}, "http://192.168.1.5:8080/api/localsend/v2"},
		{Device{IP: "fe80::1", Port: 53317, Protocol: "https"}, "https://[fe80::1]:53317/api/localsend/v2"},
	}
	for _, test := range tests {
		if got := test.device.BaseURL(); got != test.want {
			t.Errorf("BaseURL() = %q, want %q", got, test.want)
		}
	}
}

func TestDeviceFromMulticastDefaults(t *testing.T) {
	device := DeviceFromMulticast(Multicast{Alias: "Phone"}, "10.0.0.2")
	if device.Port != DefaultPort {
		t.Errorf("port = %d, want the default %d", device.Port, DefaultPort)
	}
	if device.Protocol != ProtocolHTTPS {
		t.Errorf("protocol = %q, want %q", device.Protocol, ProtocolHTTPS)
	}
	if device.IP != "10.0.0.2" {
		t.Errorf("ip = %q", device.IP)
	}
}
