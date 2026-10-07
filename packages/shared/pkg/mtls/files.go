package mtls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/shared/pkg/env"
	"github.com/e2b-dev/infra/packages/shared/pkg/logger"
)

// Environment variables naming the files, and the paths the chart mounts them at.
const (
	CertFileEnv = "E2B_TLS_CERT_FILE"
	KeyFileEnv  = "E2B_TLS_KEY_FILE"
	CAFileEnv   = "E2B_TLS_CA_FILE"

	DefaultCertFile = "/var/run/e2b/tls/tls.crt"
	DefaultKeyFile  = "/var/run/e2b/tls/tls.key"
	DefaultCAFile   = "/var/run/e2b/trust/ca.crt"

	// DefaultReloadInterval is how often the files are re-read. A renewal
	// written by the certificate manager reaches new handshakes within it.
	DefaultReloadInterval = time.Minute
)

// FileConfig names the certificate, its key and the trust bundle.
type FileConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// FileConfigFromEnv reads the three paths from the environment, defaulting each.
func FileConfigFromEnv() FileConfig {
	return FileConfig{
		CertFile: env.GetEnv(CertFileEnv, DefaultCertFile),
		KeyFile:  env.GetEnv(KeyFileEnv, DefaultKeyFile),
		CAFile:   env.GetEnv(CAFileEnv, DefaultCAFile),
	}
}

// Bundle is one consistent set of loaded material.
type Bundle struct {
	// Certificate has Leaf populated; Certificate.Certificate[1:] are the
	// intermediates the certificate file carried.
	Certificate *tls.Certificate
	// Roots verifies peers; RootCerts are the same certificates for reporting.
	Roots     *x509.CertPool
	RootCerts []*x509.Certificate
	LoadedAt  time.Time
}

// Files reads the certificate, key and trust bundle from disk, re-reads them
// on a timer and on demand, and serves the last good set. A read that fails
// or is inconsistent keeps the previous set; the process starts whatever the
// files hold.
type Files struct {
	cfg      FileConfig
	interval time.Duration
	log      logger.Logger
	metrics  *Metrics

	mu         sync.Mutex
	current    atomic.Pointer[Bundle]
	lastDigest [sha256.Size]byte
}

// FilesOption configures NewFiles.
type FilesOption func(*Files)

// WithReloadInterval replaces DefaultReloadInterval.
func WithReloadInterval(interval time.Duration) FilesOption {
	return func(f *Files) { f.interval = interval }
}

// WithFilesLogger replaces logger.L().
func WithFilesLogger(log logger.Logger) FilesOption {
	return func(f *Files) { f.log = log }
}

// WithFilesMetrics replaces DefaultMetrics().
func WithFilesMetrics(m *Metrics) FilesOption {
	return func(f *Files) { f.metrics = m }
}

func (f *Files) observe() *Metrics {
	if f.metrics != nil {
		return f.metrics
	}

	return DefaultMetrics()
}

// NewFiles loads the files once and returns. It never fails: with missing
// or inconsistent files Bundle is nil until a later reload succeeds, and the
// failure is logged.
func NewFiles(ctx context.Context, cfg FileConfig, opts ...FilesOption) *Files {
	f := &Files{cfg: cfg, interval: DefaultReloadInterval, log: logger.L()}
	for _, o := range opts {
		o(f)
	}
	// A ticker panics on a non-positive interval; the default stands in.
	if f.interval <= 0 {
		f.interval = DefaultReloadInterval
	}

	// Reload logs its own failure; the process starts regardless.
	_ = f.Reload(ctx)

	return f
}

// Start re-reads the files every interval until ctx ends.
func (f *Files) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(f.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = f.Reload(ctx)
			}
		}
	}()
}

// Bundle returns the current set, or nil when nothing has loaded yet.
func (f *Files) Bundle() *Bundle {
	if f == nil {
		return nil
	}

	return f.current.Load()
}

// Reload reads all three files now. The new set replaces the old only when
// the key matches the certificate and every PEM block of the certificate
// file and the trust bundle is a certificate that parses.
func (f *Files) Reload(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	certPEM, keyPEM, caPEM, err := f.read()
	if err != nil {
		return f.failed(ctx, err)
	}

	digest := sha256.Sum256(bytes.Join([][]byte{certPEM, keyPEM, caPEM}, []byte{0}))
	previous := f.current.Load()
	if previous != nil && digest == f.lastDigest {
		f.observe().reload(ctx, ReloadUnchanged)

		return nil
	}

	bundle, err := parseBundle(certPEM, keyPEM, caPEM)
	if err != nil {
		return f.failed(ctx, err)
	}

	f.current.Store(bundle)
	f.lastDigest = digest
	f.observe().reload(ctx, ReloadLoaded)
	f.observe().bundleLoaded(ctx, previous, bundle)
	f.log.Info(ctx, "mtls: certificate files loaded",
		zap.String("subject", bundle.Certificate.Leaf.Subject.String()),
		logger.Time("not_after", bundle.Certificate.Leaf.NotAfter),
		zap.Int("chain_length", len(bundle.Certificate.Certificate)),
		zap.Int("roots", len(bundle.RootCerts)))

	return nil
}

func (f *Files) read() ([]byte, []byte, []byte, error) {
	certPEM, err := os.ReadFile(f.cfg.CertFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(f.cfg.KeyFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read key: %w", err)
	}
	caPEM, err := os.ReadFile(f.cfg.CAFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read trust bundle: %w", err)
	}

	return certPEM, keyPEM, caPEM, nil
}

func (f *Files) failed(ctx context.Context, err error) error {
	f.observe().reload(ctx, ReloadFailed)
	if f.current.Load() == nil {
		f.log.Warn(ctx, "mtls: no certificate loaded; TLS handshakes fail until the files are fixed",
			zap.Error(err),
			zap.String("cert_file", f.cfg.CertFile),
			zap.String("key_file", f.cfg.KeyFile),
			zap.String("ca_file", f.cfg.CAFile))
	} else {
		f.log.Warn(ctx, "mtls: certificate reload failed; keeping the previous set", zap.Error(err))
	}

	return err
}

// parseBundle parses the three files into one set or fails as a whole.
func parseBundle(certPEM, keyPEM, caPEM []byte) (*Bundle, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate and key: %w", err)
	}
	if blocks := countPEMBlocks(certPEM); blocks != len(cert.Certificate) {
		return nil, fmt.Errorf("%w: certificate file has %d PEM blocks but %d certificates parsed", ErrInvalidBundle, blocks, len(cert.Certificate))
	}
	// X509KeyPair parses only the leaf and keeps the rest of the chain as bytes.
	for i, der := range cert.Certificate[1:] {
		if _, err := x509.ParseCertificate(der); err != nil {
			return nil, fmt.Errorf("%w: certificate %d: %w", ErrInvalidBundle, i+2, err)
		}
	}

	roots, rootCerts, err := parseTrustBundle(caPEM)
	if err != nil {
		return nil, err
	}

	return &Bundle{Certificate: &cert, Roots: roots, RootCerts: rootCerts, LoadedAt: time.Now()}, nil
}

// parseTrustBundle walks every PEM block: each must be a certificate that
// parses, and there must be at least one. pem.Decode skips a block whose
// body does not decode, so the block headers are counted as well.
func parseTrustBundle(caPEM []byte) (*x509.CertPool, []*x509.Certificate, error) {
	pool := x509.NewCertPool()

	var certs []*x509.Certificate
	for rest := caPEM; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("%w: found a %q block", ErrInvalidBundle, block.Type)
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: certificate %d: %w", ErrInvalidBundle, len(certs)+1, err)
		}

		pool.AddCert(cert)
		certs = append(certs, cert)
	}

	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("%w: no certificate found", ErrInvalidBundle)
	}
	if blocks := countPEMBlocks(caPEM); blocks != len(certs) {
		return nil, nil, fmt.Errorf("%w: %d PEM blocks but %d certificates parsed", ErrInvalidBundle, blocks, len(certs))
	}

	return pool, certs, nil
}

var pemBegin = []byte("-----BEGIN ")

// countPEMBlocks counts block headers, including those pem.Decode skips.
func countPEMBlocks(data []byte) int {
	return bytes.Count(data, pemBegin)
}
