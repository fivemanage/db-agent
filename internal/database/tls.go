package database

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/fivemanage/db-agent/internal/config"
)

// VerifiedTLSFingerprint returns a pin for the authenticated peer certificate.
// Some MariaDB dump clients don't check the certificate's identity on loopback.
func VerifiedTLSFingerprint(ctx context.Context, settings config.DatabaseConfig) (string, error) {
	if settings.TLS.Mode != config.TLSVerifyIdentity {
		return "", errors.New("a database certificate fingerprint requires verified TLS")
	}
	var fingerprint string
	connector, err := databaseConnector(settings, Timeout, func(state tls.ConnectionState) error {
		digest := sha256.Sum256(state.VerifiedChains[0][0].Raw)
		fingerprint = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	conn, err := connector.Connect(ctx)
	if err != nil {
		return "", errors.New(Explain(err))
	}
	defer conn.Close()

	if fingerprint == "" {
		return "", errors.New("database TLS connection did not present a verified certificate")
	}
	return fingerprint, nil
}

func databaseRootCAs(path string) (*x509.CertPool, error) {
	const maxSize = 1 << 20
	invalid := errors.New("cannot load the database TLS CA; use a readable PEM CA certificate file")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSize {
		return nil, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()

	pem, err := io.ReadAll(io.LimitReader(file, maxSize+1))
	roots := x509.NewCertPool()
	if err != nil || len(pem) > maxSize || !roots.AppendCertsFromPEM(pem) {
		return nil, invalid
	}
	return roots, nil
}
