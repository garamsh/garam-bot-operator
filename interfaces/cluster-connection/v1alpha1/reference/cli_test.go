package reference_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// syncBuffer is a buffer serve may write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// served runs serve in dir until the test ends, answering its ready line.
func served(t *testing.T, dir string) reference.Ready {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- reference.Main(ctx, []string{"--dir", dir, "serve"}, stdout, stderr) }()
	t.Cleanup(func() {
		cancel()
		assert.Equal(t, 0, <-exited, stderr.String())
	})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "\n") {
		require.True(t, time.Now().Before(deadline), "serve printed no ready line: %s", stderr.String())
		time.Sleep(10 * time.Millisecond)
	}
	var ready reference.Ready
	require.NoError(t, json.Unmarshal([]byte(stdout.String()), &ready))
	return ready
}

// fixture runs one fixture subcommand against the server in dir.
func fixture(t *testing.T, dir string, args ...string) (int, map[string]any) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := reference.Main(context.Background(), append([]string{"--dir", dir}, args...), &stdout, &stderr)
	var out map[string]any
	if stdout.Len() > 0 {
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &out), stdout.String())
	}
	return code, out
}

func TestMain_ServesTLSFromGeneratedRootAndRunsFixture(t *testing.T) {
	dir, err := os.MkdirTemp("", "ccr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ready := served(t, dir)

	root, err := os.ReadFile(ready.TrustFile)
	require.NoError(t, err)
	written, err := os.ReadFile(filepath.Join(dir, reference.URLFile))
	require.NoError(t, err)
	assert.Equal(t, ready.BaseURL, strings.TrimSpace(string(written)))
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(root))
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	untrusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}}
	_, err = untrusting.Get(ready.BaseURL + regPath)
	assert.Error(t, err, "control: the certificate verifies only against the written root")

	code, minted := fixture(t, dir, "mint-token")
	require.Equal(t, 0, code)
	grn := minted["cluster"].(string)
	encoded, err := json.Marshal(registration(minted["registrationToken"].(string), publicJWK(t, testKeys()[0])))
	require.NoError(t, err)
	resp, err := trusting.Post(ready.BaseURL+regPath, "application/json", bytes.NewReader(encoded))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	declareFile := filepath.Join(dir, "declare.json")
	object, err := json.Marshal(map[string]any{"cluster": grn, "object": bot("a")})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(declareFile, object, 0o600))
	code, declared := fixture(t, dir, "declare", declareFile)
	require.Equal(t, 0, code)
	assert.EqualValues(t, 1, declared["generation"])

	for _, args := range [][]string{
		{"tombstone", declared["id"].(string)},
		{"latest-report", grn},
		{"expire-cursors", grn},
		{"revoke-keys", grn},
		{"delete-cluster", grn},
	} {
		code, out := fixture(t, dir, args...)
		assert.Equal(t, 0, code, "%v: %v", args, out)
		assert.NotNil(t, out, "%v printed no JSON", args)
	}
	code, refused := fixture(t, dir, "latest-report", grn)
	assert.Equal(t, 1, code, "a fixture action the server refuses exits 1")
	assert.Equal(t, "not_found", refused["error"])
}

func TestMain_RefusesUnknownSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	unknown := []string{"--dir", t.TempDir(), "nonsense"}
	assert.Equal(t, 2, reference.Main(context.Background(), unknown, io.Discard, &stderr))
	assert.Equal(t, 2, reference.Main(context.Background(), []string{"serve"}, io.Discard, &stderr), "--dir is required")
}
