// Package httpx builds the HTTP clients used to talk to LocalSend peers.
package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// Options configures a client.
type Options struct {
	// Identity provides the client certificate. The HTTPS server of current
	// LocalSend versions demands one (client_auth_mandatory), so it is always
	// presented.
	Identity *identity.Identity

	// ExpectFingerprint pins the peer certificate to this fingerprint when set.
	ExpectFingerprint string

	// Timeout bounds the whole request. Leave zero for uploads, which may run
	// for minutes; the request context governs those instead.
	Timeout time.Duration
}

// New creates an HTTP client that trusts self-signed LocalSend certificates.
//
// Peer certificates are not checked against a CA — LocalSend has none. Instead
// the validity period is enforced and, when known from discovery, the
// fingerprint is pinned.
func New(opts Options) *http.Client {
	tlsCfg := &tls.Config{
		// LocalSend peers use self-signed certificates; verification happens in
		// VerifyPeerCertificate below.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPeer(rawCerts, opts.ExpectFingerprint)
		},
		MinVersion: tls.VersionTLS12,
	}
	if opts.Identity != nil {
		tlsCfg.Certificates = []tls.Certificate{opts.Identity.Certificate}
	}

	return &http.Client{
		Timeout: opts.Timeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableCompression:  true,
			MaxIdleConns:        16,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

func verifyPeer(rawCerts [][]byte, expectFingerprint string) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("peer presented no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("peer certificate: %w", err)
	}

	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return fmt.Errorf("peer certificate is not valid at this time (valid %s .. %s)",
			cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
	}

	if expectFingerprint != "" {
		got := identity.Fingerprint(rawCerts[0])
		if !strings.EqualFold(got, expectFingerprint) {
			return fmt.Errorf("peer fingerprint mismatch: expected %s, got %s", expectFingerprint, got)
		}
	}

	// LocalSend certificates are self-signed. A failure here is not fatal (a
	// peer might legitimately use a CA-issued certificate) but worth noting.
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		ui.Debugf("peer certificate is not self-signed: %v", err)
	}
	return nil
}

// StatusError reports a non-successful HTTP status from a peer.
type StatusError struct {
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s: %s", e.Status, e.Body)
	}
	return e.Status
}

// PostJSON sends in as JSON and decodes the response into out.
//
// out may be nil for endpoints without a body. A 204 response leaves out
// untouched. The status code is reported as *StatusError.
func PostJSON(ctx context.Context, client *http.Client, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return do(client, req, out)
}

// GetJSON fetches url and decodes the response into out.
func GetJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return do(client, req, out)
}

func do(client *http.Client, req *http.Request, out any) error {
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		return StatusErrorFrom(res)
	}
	if out == nil || res.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// StatusErrorFrom turns a failed response into a *StatusError, consuming a
// short prefix of the body as the message.
func StatusErrorFrom(res *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return &StatusError{
		Code:   res.StatusCode,
		Status: res.Status,
		Body:   strings.TrimSpace(string(snippet)),
	}
}

// StatusCode returns the HTTP status carried by err, or 0.
func StatusCode(err error) int {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code
	}
	return 0
}
