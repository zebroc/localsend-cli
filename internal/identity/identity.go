// Package identity manages the persistent identity of this LocalSend member:
// its TLS certificate, the fingerprint derived from it and its alias.
package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigDirEnv overrides the configuration directory. Handy for tests and for
// running a second identity on the same host.
const ConfigDirEnv = "LOCALSEND_CLI_CONFIG_DIR"

const (
	certFile   = "cert.pem"
	keyFile    = "key.pem"
	configFile = "config.json"

	// certValidity matches the reference implementation, which issues 10 year
	// certificates.
	certValidity = 10 * 365 * 24 * time.Hour
)

// Identity is the identity of this device on the network.
type Identity struct {
	Dir   string
	Alias string

	// Certificate is the TLS certificate presented as server and as client.
	Certificate tls.Certificate

	// Fingerprint is the uppercase hex SHA-256 of the certificate in DER form,
	// which is how LocalSend identifies devices in HTTPS mode.
	Fingerprint string
}

type storedConfig struct {
	Alias string `json:"alias"`
}

// ConfigDir returns the directory holding certificate and settings, creating it
// if necessary.
func ConfigDir() (string, error) {
	if dir := os.Getenv(ConfigDirEnv); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "localsend-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Load returns the identity, generating a certificate on first use.
//
// A non-empty alias overrides (and persists) the stored alias.
func Load(alias string) (*Identity, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, fmt.Errorf("config directory: %w", err)
	}

	cert, err := loadOrCreateCert(dir)
	if err != nil {
		return nil, err
	}

	stored, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	switch {
	case alias != "":
		if alias != stored.Alias {
			stored.Alias = alias
			if err := writeConfig(dir, stored); err != nil {
				return nil, err
			}
		}
	case stored.Alias == "":
		stored.Alias = defaultAlias()
		if err := writeConfig(dir, stored); err != nil {
			return nil, err
		}
	}

	return &Identity{
		Dir:         dir,
		Alias:       stored.Alias,
		Certificate: cert,
		Fingerprint: Fingerprint(cert.Certificate[0]),
	}, nil
}

// Fingerprint computes the LocalSend fingerprint of a DER encoded certificate:
// the SHA-256 digest as uppercase hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// loadOrCreateCert loads the stored key pair, or creates one when it is
// missing, unreadable or expired. An expired certificate is worthless because
// peers verify the validity period.
func loadOrCreateCert(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil && time.Now().Before(leaf.NotAfter) {
			cert.Leaf = leaf
			return cert, nil
		}
	}

	certPEM, keyPEM, err := generateCert()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	newCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	if leaf, err := x509.ParseCertificate(newCert.Certificate[0]); err == nil {
		newCert.Leaf = leaf
	}
	return newCert, nil
}

// generateCert creates a self-signed RSA-2048 certificate.
//
// RSA and the "LocalSend User" subject mirror the reference implementation.
// Peers only check that the certificate is time-valid and correctly
// self-signed, so no SAN or CA extensions are required.
func generateCert() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "LocalSend User"},
		// Backdate slightly so a peer with a skewed clock still accepts it.
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshalKey(key)})
	return certPEM, keyPEM, nil
}

func mustMarshalKey(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		// PKCS#8 marshalling of an RSA key cannot fail.
		panic(err)
	}
	return der
}

func readConfig(dir string) (storedConfig, error) {
	var cfg storedConfig
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// A damaged config should not block startup.
		return storedConfig{}, nil
	}
	return cfg, nil
}

func writeConfig(dir string, cfg storedConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, configFile), append(data, '\n'), 0o600)
}

func defaultAlias() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "LocalSend CLI"
	}
	host = strings.TrimSuffix(host, ".local")
	host = strings.TrimSuffix(host, ".lan")
	return host
}
