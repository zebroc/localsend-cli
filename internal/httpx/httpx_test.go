package httpx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
)

// selfSigned creates a certificate valid in the given window. ECDSA keeps the
// tests fast; peers only care about the validity period and the signature.
func selfSigned(t *testing.T, notBefore, notAfter time.Time) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "LocalSend User"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestVerifyPeerAcceptsValidCertificate(t *testing.T) {
	der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err := verifyPeer([][]byte{der}, ""); err != nil {
		t.Errorf("verifyPeer rejected a valid certificate: %v", err)
	}
}

// Peers enforce the validity period, so we do the same and report it clearly
// instead of failing with an opaque TLS error.
func TestVerifyPeerRejectsExpiredCertificate(t *testing.T) {
	der := selfSigned(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	err := verifyPeer([][]byte{der}, "")
	if err == nil {
		t.Fatal("verifyPeer accepted an expired certificate")
	}
	if !strings.Contains(err.Error(), "not valid at this time") {
		t.Errorf("error %q should explain the validity period", err)
	}
}

func TestVerifyPeerRejectsNotYetValidCertificate(t *testing.T) {
	der := selfSigned(t, time.Now().Add(time.Hour), time.Now().Add(48*time.Hour))
	if err := verifyPeer([][]byte{der}, ""); err == nil {
		t.Error("verifyPeer accepted a certificate that is not valid yet")
	}
}

func TestVerifyPeerPinsFingerprint(t *testing.T) {
	der := selfSigned(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	fingerprint := identity.Fingerprint(der)

	if err := verifyPeer([][]byte{der}, fingerprint); err != nil {
		t.Errorf("verifyPeer rejected the matching fingerprint: %v", err)
	}
	// Comparison must be case-insensitive: peers differ in how they spell hex.
	if err := verifyPeer([][]byte{der}, strings.ToLower(fingerprint)); err != nil {
		t.Errorf("verifyPeer rejected the same fingerprint in lowercase: %v", err)
	}

	err := verifyPeer([][]byte{der}, "0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("verifyPeer accepted a mismatching fingerprint")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error %q should mention the mismatch", err)
	}
}

func TestVerifyPeerRejectsMissingCertificate(t *testing.T) {
	if err := verifyPeer(nil, ""); err == nil {
		t.Error("verifyPeer accepted a peer without a certificate")
	}
}

func TestPostJSONDecodesResponse(t *testing.T) {
	type request struct {
		Alias string `json:"alias"`
	}
	type response struct {
		Fingerprint string `json:"fingerprint"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got request
		if err := decode(r, &got); err != nil || got.Alias != "Sender" {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"fingerprint":"ABCD"}`)
	}))
	defer srv.Close()

	var out response
	if err := PostJSON(context.Background(), srv.Client(), srv.URL, request{Alias: "Sender"}, &out); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if out.Fingerprint != "ABCD" {
		t.Errorf("fingerprint = %q, want %q", out.Fingerprint, "ABCD")
	}
}

// 204 means "nothing to transfer" in the protocol and carries no body, so it
// must not be treated as a decoding error.
func TestPostJSONHandlesNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out := struct {
		SessionID string `json:"sessionId"`
	}{}
	if err := PostJSON(context.Background(), srv.Client(), srv.URL, nil, &out); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if out.SessionID != "" {
		t.Errorf("sessionId = %q, want it untouched", out.SessionID)
	}
}

func TestPostJSONReportsStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "PIN required / Invalid PIN", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := PostJSON(context.Background(), srv.Client(), srv.URL, nil, nil)
	if err == nil {
		t.Fatal("PostJSON accepted a 401 response")
	}
	if got := StatusCode(err); got != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", got)
	}

	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("error %v is not a *StatusError", err)
	}
	if !strings.Contains(statusErr.Body, "PIN") {
		t.Errorf("body = %q, want the peer's message", statusErr.Body)
	}
}

func TestGetJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, `{"alias":"Nice Orange"}`)
	}))
	defer srv.Close()

	out := struct {
		Alias string `json:"alias"`
	}{}
	if err := GetJSON(context.Background(), srv.Client(), srv.URL, &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if out.Alias != "Nice Orange" {
		t.Errorf("alias = %q", out.Alias)
	}
}

// StatusCode is used to turn protocol errors into advice, so it must not claim
// a status for unrelated failures such as a dropped connection.
func TestStatusCodeOfPlainError(t *testing.T) {
	if got := StatusCode(errors.New("connection refused")); got != 0 {
		t.Errorf("StatusCode = %d, want 0", got)
	}
	wrapped := fmt.Errorf("upload: %w", &StatusError{Code: 409, Status: "409 Conflict"})
	if got := StatusCode(wrapped); got != 409 {
		t.Errorf("StatusCode of a wrapped error = %d, want 409", got)
	}
}

func TestNewPresentsClientCertificate(t *testing.T) {
	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	id, err := identity.Load("Sender")
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}

	// Current LocalSend versions demand a client certificate, so the client
	// must always offer one.
	client := New(Options{Identity: id})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", client.Transport)
	}
	if len(transport.TLSClientConfig.Certificates) != 1 {
		t.Error("client does not present a certificate")
	}
	if transport.TLSClientConfig.VerifyPeerCertificate == nil {
		t.Error("peer verification is not wired up")
	}
}

func decode(r *http.Request, out any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(out)
}
