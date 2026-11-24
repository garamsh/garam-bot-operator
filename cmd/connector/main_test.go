package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rootOf writes server's certificate to a PEM file, as the root a deployer configures.
func rootOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "root.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

func serve(t *testing.T, maxVersion uint16, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MaxVersion: maxVersion}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// selfSigned is a certificate for 127.0.0.1 under a root of its own, unlike every httptest server's,
// which share one.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "another root"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestGaramClient_TrustsTheConfiguredRootAlone(t *testing.T) {
	garam := serve(t, tls.VersionTLS13, ok)
	other := httptest.NewUnstartedServer(http.HandlerFunc(ok))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	t.Cleanup(other.Close)
	client, err := garamClient(rootOf(t, garam))
	require.NoError(t, err)

	response, err := client.Get(garam.URL)
	require.NoError(t, err, "control: the configured root's server is reached")
	_ = response.Body.Close()
	_, err = client.Get(other.URL)

	require.ErrorContains(t, err, "unknown authority", "T-2: a server another root signed is refused")
}

func TestGaramClient_RefusesTLSBelow1_3(t *testing.T) {
	garam := serve(t, tls.VersionTLS12, ok)
	client, err := garamClient(rootOf(t, garam))
	require.NoError(t, err)

	_, err = client.Get(garam.URL)

	require.Error(t, err)
}

func TestGaramClient_FollowsNoRedirect(t *testing.T) {
	followed := false
	var garam *httptest.Server
	garam = serve(t, tls.VersionTLS13, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, garam.URL+"/elsewhere", http.StatusTemporaryRedirect)
	})
	client, err := garamClient(rootOf(t, garam))
	require.NoError(t, err)

	response, err := client.Post(garam.URL+"/registrations", "application/json", nil)

	require.NoError(t, err)
	_ = response.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	assert.False(t, followed, "the request, and the token it carries, is not sent on")
}

func TestGaramClient_KeepsTheEnvironmentsProxy(t *testing.T) {
	client, err := garamClient(rootOf(t, serve(t, tls.VersionTLS13, ok)))
	require.NoError(t, err)

	transport, isTransport := client.Transport.(*http.Transport)

	require.True(t, isTransport)
	assert.NotNil(t, transport.Proxy, "HTTPS_PROXY is honoured")
}

func TestWatchedNamespaces_ReadsEachNameOnceWithoutBlanks(t *testing.T) {
	assert.Equal(t, []string{"bots", "more-bots"}, watchedNamespaces(" bots, ,more-bots,bots,"))
	assert.Empty(t, watchedNamespaces(" , "), "no name at all leaves the required flag unset")
}
