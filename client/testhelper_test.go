package client

import (
	"crypto/x509"
	"os"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// certPoolWithCustomTLSCertificate returns an x509.CertPool with certFile appended to it. This
// should be used in tests rather than setting the SSL_CERT_FILE environment variable, as the
// the environment variable is cached per test binary. This breaks test binaries with multiple
// tests that need to override SSL_CERT_FILE.
func certPoolWithCustomTLSCertificate(t *testing.T, certFile string) *x509.CertPool {
	caCert, err := os.ReadFile(certFile)
	require.NoError(t, err)
	caCertPool := x509.NewCertPool()
	require.True(t, caCertPool.AppendCertsFromPEM(caCert))
	return caCertPool
}

func TestMain(m *testing.M) {
	testhelper.Run(m)
}
