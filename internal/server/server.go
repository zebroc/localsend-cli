// Package server implements the receiving side of the LocalSend protocol:
// discovery registration and the upload API.
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// maxRequestBody caps the JSON bodies we read. A transfer of 100k files still
// fits comfortably.
const maxRequestBody = 32 << 20

// Config configures a Server.
type Config struct {
	// Identity supplies alias, fingerprint and TLS certificate.
	Identity *identity.Identity

	// Port to listen on. Zero picks a free port, which is what the sender does
	// while it waits for registration callbacks.
	Port int

	// UseTLS serves HTTPS. Announcements must advertise the same choice.
	UseTLS bool

	// DstDir is where received files are stored. When empty the server accepts
	// no uploads and answers prepare-upload with 403; used for discovery only.
	DstDir string

	// PIN, when set, must be supplied as ?pin= on prepare-upload.
	PIN string

	// OnPeer is called for every device that registers with us or sends to us.
	OnPeer func(proto.Device)
}

// Server serves the LocalSend HTTP API.
type Server struct {
	cfg      Config
	listener net.Listener
	http     *http.Server
	sessions *sessionStore
	pins     *pinGuard
	receiver *Receiver
}

// New binds the listener so that Port reports the real port, but does not serve
// yet.
func New(cfg Config) (*Server, error) {
	// Explicitly IPv4: LocalSend discovery is IPv4-only (the multicast group is
	// 224.0.0.167) and the official app binds IPv4 as well. A dual-stack "tcp"
	// listener would bind [::]:port successfully next to the app's IPv4 socket
	// and then silently receive nothing, which is far worse than failing here.
	listener, err := net.Listen("tcp4", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("port %d is already in use — another LocalSend instance is probably "+
				"running; pass --port to use a different one: %w", cfg.Port, err)
		}
		return nil, fmt.Errorf("listen on port %d: %w", cfg.Port, err)
	}

	s := &Server{
		cfg:      cfg,
		listener: listener,
		sessions: newSessionStore(),
		pins:     newPinGuard(),
	}
	if cfg.DstDir != "" {
		s.receiver = &Receiver{Dir: cfg.DstDir}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.BasePath+"/register", s.handleRegister)
	mux.HandleFunc("GET "+proto.BasePath+"/info", s.handleInfo)
	mux.HandleFunc("POST "+proto.BasePath+"/prepare-upload", s.handlePrepareUpload)
	mux.HandleFunc("POST "+proto.BasePath+"/upload", s.handleUpload)
	mux.HandleFunc("POST "+proto.BasePath+"/cancel", s.handleCancel)

	s.http = &http.Server{
		Handler: mux,
		// No ReadTimeout: uploads of large files legitimately take minutes.
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// Keep net/http's own chatter (aborted TLS handshakes and the like) out
		// of the normal output.
		ErrorLog: log.New(ui.DebugWriter{}, "", 0),
	}
	if cfg.UseTLS {
		s.http.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cfg.Identity.Certificate},
			MinVersion:   tls.VersionTLS12,
			// Senders are asked for a certificate — the fingerprint proves who
			// they are — but a sender without one is still served.
			ClientAuth: tls.RequestClientCert,
		}
	}
	return s, nil
}

// Port returns the port the server listens on.
func (s *Server) Port() int {
	if addr, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return s.cfg.Port
}

// Protocol returns "https" or "http" as announced to peers.
func (s *Server) Protocol() string {
	if s.cfg.UseTLS {
		return proto.ProtocolHTTPS
	}
	return proto.ProtocolHTTP
}

// Serve handles requests until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.http.Close()
		}
	}()

	go s.sessions.reapLoop(ctx)

	var err error
	if s.cfg.UseTLS {
		err = s.http.ServeTLS(s.listener, "", "")
	} else {
		err = s.http.Serve(s.listener)
	}
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the server immediately.
func (s *Server) Close() error { return s.http.Close() }

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var payload proto.Register
	if err := decodeJSON(w, r, &payload); err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}

	device := proto.DeviceFromRegister(payload, clientIP(r))
	if fingerprint := certFingerprint(r); fingerprint != "" {
		// The certificate proves the identity; the body only claims it.
		device.Fingerprint = fingerprint
	}
	s.notifyPeer(device)
	ui.Debugf("registered by %s", device)

	writeJSON(w, http.StatusOK, s.registerResponse())
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registerResponse())
}

func (s *Server) registerResponse() proto.RegisterResponse {
	return proto.RegisterResponse{
		Alias:       s.cfg.Identity.Alias,
		Version:     proto.Version,
		DeviceType:  proto.DeviceTypeHeadless,
		Fingerprint: s.cfg.Identity.Fingerprint,
		Download:    false,
	}
}

func (s *Server) handlePrepareUpload(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	if s.cfg.PIN != "" {
		switch s.pins.check(ip, r.URL.Query().Get("pin"), s.cfg.PIN) {
		case pinTooMany:
			ui.Warnf("too many wrong PINs from %s", ip)
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		case pinWrong:
			ui.Warnf("wrong PIN from %s", ip)
			http.Error(w, "PIN required / Invalid PIN", http.StatusUnauthorized)
			return
		}
	}

	var payload proto.PrepareUploadRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		http.Error(w, "Invalid body", http.StatusBadRequest)
		return
	}
	if len(payload.Files) == 0 {
		http.Error(w, "No files provided", http.StatusBadRequest)
		return
	}
	for id, file := range payload.Files {
		if file.Size < 0 {
			ui.Warnf("rejecting transfer from %s: file %q has a negative size", ip, id)
			http.Error(w, "Invalid body", http.StatusBadRequest)
			return
		}
	}

	device := proto.DeviceFromRegister(payload.Info, ip)
	if fingerprint := certFingerprint(r); fingerprint != "" {
		device.Fingerprint = fingerprint
	}
	s.notifyPeer(device)

	if s.receiver == nil {
		ui.Debugf("rejecting transfer from %s: no destination directory", device)
		http.Error(w, "Rejected", http.StatusForbidden)
		return
	}

	// Auto-accept: that is the entire point of the daemon.
	session, tokens := s.sessions.create(ip, payload.Info.Alias, payload.Files)

	var totalBytes int64
	for _, f := range payload.Files {
		totalBytes += f.Size
	}
	ui.Infof("incoming: %d file(s), %s from %s", len(payload.Files), ui.FormatBytes(totalBytes), device)

	writeJSON(w, http.StatusOK, proto.PrepareUploadResponse{
		SessionID: session.id,
		Files:     tokens,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	sessionID := query.Get("sessionId")
	fileID := query.Get("fileId")
	token := query.Get("token")

	if sessionID == "" || fileID == "" || token == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}
	if s.receiver == nil {
		http.Error(w, "Invalid token or IP address", http.StatusForbidden)
		return
	}

	file, err := s.sessions.beginUpload(sessionID, fileID, token, clientIP(r))
	if err != nil {
		ui.Debugf("rejecting upload from %s: %v", clientIP(r), err)
		http.Error(w, "Invalid token or IP address", http.StatusForbidden)
		return
	}

	start := time.Now()
	path, saveErr := s.receiver.Save(file, r.Body)

	// Log the file before closing the session, otherwise the session summary
	// would appear before the last file it counts.
	if saveErr == nil {
		ui.Infof("received %s (%s in %s)", path, ui.FormatBytes(file.Size), ui.FormatDuration(time.Since(start)))
	} else {
		ui.Errorf("%s: %v", file.FileName, saveErr)
	}
	s.sessions.finishUpload(sessionID, fileID, saveErr == nil)

	if saveErr != nil {
		http.Error(w, "Unknown error by receiver", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID != "" && s.sessions.cancel(sessionID, clientIP(r)) {
		ui.Infof("transfer cancelled by sender")
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) notifyPeer(device proto.Device) {
	if s.cfg.OnPeer != nil && device.Alias != "" && device.IP != "" {
		s.cfg.OnPeer(device)
	}
}

// clientIP extracts the peer address of the connection.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// certFingerprint returns the fingerprint of the client certificate presented
// during the TLS handshake, or "" when the sender presented none.
func certFingerprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return identity.Fingerprint(r.TLS.PeerCertificates[0].Raw)
}

// decodeJSON reads a request body, capped so a peer cannot exhaust memory with
// an endless "JSON" document.
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		ui.Debugf("writing response: %v", err)
	}
}

// pinGuard rate-limits PIN attempts per IP.
type pinGuard struct {
	mu       sync.Mutex
	failures map[string]*pinFailures
}

type pinFailures struct {
	count int
	last  time.Time
}

type pinResult int

const (
	pinOK pinResult = iota
	pinWrong
	pinTooMany
)

const (
	pinMaxFailures = 5
	pinLockout     = 5 * time.Minute
)

func newPinGuard() *pinGuard {
	return &pinGuard{failures: map[string]*pinFailures{}}
}

func (g *pinGuard) check(ip, got, want string) pinResult {
	g.mu.Lock()
	defer g.mu.Unlock()

	state := g.failures[ip]
	if state != nil {
		if time.Since(state.last) > pinLockout {
			delete(g.failures, ip)
			state = nil
		} else if state.count >= pinMaxFailures {
			state.last = time.Now()
			return pinTooMany
		}
	}

	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		delete(g.failures, ip)
		return pinOK
	}

	if state == nil {
		state = &pinFailures{}
		g.failures[ip] = state
	}
	state.count++
	state.last = time.Now()
	return pinWrong
}
