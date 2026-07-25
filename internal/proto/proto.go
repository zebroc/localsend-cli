// Package proto contains the wire format of the LocalSend protocol v2.1.
//
// See ~/Downloads/protocol/README.md for the specification.
package proto

import (
	"fmt"
	"net"
	"strconv"
)

const (
	// DefaultPort is used for both the HTTP server (TCP) and multicast (UDP).
	DefaultPort = 53317

	// DefaultMulticastGroup is 224.0.0.167 because some Android devices reject
	// any group outside of 224.0.0.0/24.
	DefaultMulticastGroup = "224.0.0.167"

	// Version is the protocol version we announce.
	Version = "2.1"

	// BasePath prefixes every API route.
	BasePath = "/api/localsend/v2"

	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"

	// DeviceTypeHeadless is what we announce: a program without GUI running on
	// a terminal.
	DeviceTypeHeadless = "headless"
)

// Multicast is the announcement/response message sent to the multicast group.
//
// Announcement and Announce carry the same value: v1 used "announcement",
// v2 uses "announce". The reference implementation writes both, so we do too.
type Multicast struct {
	Alias        string `json:"alias"`
	Version      string `json:"version"`
	DeviceModel  string `json:"deviceModel,omitempty"`
	DeviceType   string `json:"deviceType,omitempty"`
	Fingerprint  string `json:"fingerprint"`
	Port         int    `json:"port"`
	Protocol     string `json:"protocol"`
	Download     bool   `json:"download"`
	Announce     bool   `json:"announce"`
	Announcement bool   `json:"announcement"`
}

// IsAnnouncement reports whether this message asks for a response.
func (m Multicast) IsAnnouncement() bool { return m.Announce || m.Announcement }

// Register is the body of POST /register and the "info" field of prepare-upload.
type Register struct {
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel,omitempty"`
	DeviceType  string `json:"deviceType,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Download    bool   `json:"download"`
}

// RegisterResponse is the body returned by POST /register and GET /info.
// It carries no port: the caller already knows it.
type RegisterResponse struct {
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel,omitempty"`
	DeviceType  string `json:"deviceType,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Download    bool   `json:"download"`
}

// FileMetadata carries optional timestamps in RFC 3339.
type FileMetadata struct {
	Modified string `json:"modified,omitempty"`
	Accessed string `json:"accessed,omitempty"`
}

// File is the metadata of a single file offered in a transfer.
//
// FileName carries the relative path with "/" separators when a directory is
// sent, e.g. "Photos/2024/beach.jpg". FileType is a MIME type; receivers also
// accept the legacy v1 enum ("image", "video", …).
type File struct {
	ID       string        `json:"id"`
	FileName string        `json:"fileName"`
	Size     int64         `json:"size"`
	FileType string        `json:"fileType"`
	SHA256   string        `json:"sha256,omitempty"`
	Preview  string        `json:"preview,omitempty"`
	Metadata *FileMetadata `json:"metadata,omitempty"`
}

// PrepareUploadRequest is the body of POST /prepare-upload.
type PrepareUploadRequest struct {
	Info  Register        `json:"info"`
	Files map[string]File `json:"files"`
}

// PrepareUploadResponse maps every accepted file ID to its upload token.
type PrepareUploadResponse struct {
	SessionID string            `json:"sessionId"`
	Files     map[string]string `json:"files"`
}

// Device is a discovered LocalSend peer.
type Device struct {
	Alias       string `json:"alias"`
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Fingerprint string `json:"fingerprint"`
	DeviceModel string `json:"deviceModel,omitempty"`
	DeviceType  string `json:"deviceType,omitempty"`
	Version     string `json:"version,omitempty"`
	Download    bool   `json:"download,omitempty"`
}

// BaseURL returns the API prefix of the device, e.g.
// "https://192.168.1.5:53317/api/localsend/v2".
func (d Device) BaseURL() string {
	scheme := ProtocolHTTPS
	if d.Protocol == ProtocolHTTP {
		scheme = ProtocolHTTP
	}
	return scheme + "://" + net.JoinHostPort(d.IP, strconv.Itoa(d.Port)) + BasePath
}

func (d Device) String() string {
	return fmt.Sprintf("%s (%s:%d)", d.Alias, d.IP, d.Port)
}

// DeviceFromMulticast builds a Device from an announcement received from ip.
func DeviceFromMulticast(m Multicast, ip string) Device {
	return Device{
		Alias:       m.Alias,
		IP:          ip,
		Port:        portOrDefault(m.Port),
		Protocol:    protocolOrDefault(m.Protocol),
		Fingerprint: m.Fingerprint,
		DeviceModel: m.DeviceModel,
		DeviceType:  m.DeviceType,
		Version:     m.Version,
		Download:    m.Download,
	}
}

// DeviceFromRegister builds a Device from a register request received from ip.
func DeviceFromRegister(r Register, ip string) Device {
	return Device{
		Alias:       r.Alias,
		IP:          ip,
		Port:        portOrDefault(r.Port),
		Protocol:    protocolOrDefault(r.Protocol),
		Fingerprint: r.Fingerprint,
		DeviceModel: r.DeviceModel,
		DeviceType:  r.DeviceType,
		Version:     r.Version,
		Download:    r.Download,
	}
}

// DeviceFromResponse builds a Device from a /register or /info response of a
// peer we contacted at ip:port over the given protocol.
func DeviceFromResponse(r RegisterResponse, ip string, port int, protocol string) Device {
	return Device{
		Alias:       r.Alias,
		IP:          ip,
		Port:        portOrDefault(port),
		Protocol:    protocolOrDefault(protocol),
		Fingerprint: r.Fingerprint,
		DeviceModel: r.DeviceModel,
		DeviceType:  r.DeviceType,
		Version:     r.Version,
		Download:    r.Download,
	}
}

func portOrDefault(port int) int {
	if port <= 0 || port > 65535 {
		return DefaultPort
	}
	return port
}

func protocolOrDefault(protocol string) string {
	if protocol == ProtocolHTTP {
		return ProtocolHTTP
	}
	return ProtocolHTTPS
}
