package identity

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// referenceCert is the certificate used in the LocalSend reference
// implementation's own test (packages/core/src/crypto/cert.rs), together with
// the fingerprint it expects. It pins our fingerprint format — uppercase hex
// SHA-256 over the DER bytes — to the format real peers use.
const referenceCert = `-----BEGIN CERTIFICATE-----
MIIDGTCCAgGgAwIBAgIBATANBgkqhkiG9w0BAQsFADBQMRcwFQYDVQQDEw5Mb2Nh
bFNlbmQgVXNlcjEJMAcGA1UEChMAMQkwBwYDVQQLEwAxCTAHBgNVBAcTADEJMAcG
A1UECBMAMQkwBwYDVQQGEwAwHhcNMjUwMjA5MDAwMzE0WhcNMzUwMjA3MDAwMzE0
WjBQMRcwFQYDVQQDEw5Mb2NhbFNlbmQgVXNlcjEJMAcGA1UEChMAMQkwBwYDVQQL
EwAxCTAHBgNVBAcTADEJMAcGA1UECBMAMQkwBwYDVQQGEwAwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQCL24MxhGfrdJm0Q8ZGiBkZ27ldcEChB4w7rSbJ
yiKeosoNbJl2kyj5dZjfBhWGgDLGDMM5w+Mh/5SrWgTL/QrhbB+lsrxILLznWqBi
R8wJP0P2YW9fBahQskJQcUXt/3jsCsMTWea4rWc3HZGh03bAkJfLM+PDSOfTpvAZ
6DQSp9QLzC9bgVNnq3W0SvOZGpF0xRa4InCyTUgxsNsV4+GIrmN5w4EbRFVVYu7D
5OS5fxNSCukiS0fb6oQzUp0vIAycvvWHHbAy8T6UMoUor2nfvNcryiaOX5WBMLyh
yMZ5gMOyXjdm1bT1XSlvtXPYUzxvsGAzTqS8mXjw8h7mm5htAgMBAAEwDQYJKoZI
hvcNAQELBQADggEBABZ+I7D6wkeSrsi1NBLP2zoZ5oGh+INNcGTravfOQHs4Fbas
/CysaUYjsD3fmaDh4MxgWEqAmWnnBiojfpGX2SGuFqRBKyT9DgitBt0L7Ezg1k3h
bfSiFW4hXWp75grVO8xfML7ZcWMlhKrOsOMUGiy1qs3qsyJ3w7B2Tz78HhXGO5dd
jyPmZarhixKO92UpEvKGxjO0E/3UUNUzxKTAAgFfhKpuwHUgIijM/EppZtA8OcSh
fEztiV0xKfcPVx4d6dqRt/NMElK1Ivw2vUuxTymphZkkFOzht9m73/kyKaeFp8Ij
VRus1zGVD8IVpIdPMyz01WJyS7M0fWaHXKWo+Bo=
-----END CERTIFICATE-----`

const referenceFingerprint = "4BADDE53A7F7CDEEED93189FD898E02BF6B4806CA4C05DE0ACE08319B86552FA"

func TestFingerprintMatchesReference(t *testing.T) {
	block, _ := pem.Decode([]byte(referenceCert))
	if block == nil {
		t.Fatal("could not decode the reference certificate")
	}
	if got := Fingerprint(block.Bytes); got != referenceFingerprint {
		t.Errorf("Fingerprint() = %s, want %s", got, referenceFingerprint)
	}
}

func TestLoadCreatesAndReusesIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)

	first, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if first.Alias == "" {
		t.Error("alias is empty")
	}
	if len(first.Fingerprint) != 64 {
		t.Errorf("fingerprint %q is not 64 hex characters", first.Fingerprint)
	}

	for _, name := range []string{"cert.pem", "key.pem", "config.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", name, perm)
		}
	}

	// A second load must reuse the certificate, otherwise peers would see a new
	// device on every invocation.
	second, err := Load("")
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Errorf("fingerprint changed between loads: %s != %s", second.Fingerprint, first.Fingerprint)
	}
	if second.Alias != first.Alias {
		t.Errorf("alias changed between loads: %q != %q", second.Alias, first.Alias)
	}
}

func TestLoadPersistsAliasOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)

	if _, err := Load("Workstation"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	reloaded, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Alias != "Workstation" {
		t.Errorf("alias = %q, want %q", reloaded.Alias, "Workstation")
	}
}

// TestGeneratedCertIsAcceptedByPeers checks the properties peers verify: the
// certificate must be time-valid and correctly self-signed.
func TestGeneratedCertIsAcceptedByPeers(t *testing.T) {
	t.Setenv(ConfigDirEnv, t.TempDir())

	id, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cert, err := x509.ParseCertificate(id.Certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		t.Errorf("certificate is not valid now (%s .. %s)", cert.NotBefore, cert.NotAfter)
	}
	if remaining := time.Until(cert.NotAfter); remaining < 9*365*24*time.Hour {
		t.Errorf("certificate expires in %s, want about 10 years", remaining)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Errorf("certificate is not self-signed: %v", err)
	}
	if cert.Subject.CommonName != "LocalSend User" {
		t.Errorf("common name = %q, want %q", cert.Subject.CommonName, "LocalSend User")
	}
}
