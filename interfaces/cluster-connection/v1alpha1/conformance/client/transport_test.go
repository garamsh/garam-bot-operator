//go:build conformance

package client

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/suite"
	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

func verifiesConfiguredRoot(t *testing.T) {
	h := newHarness(t)
	otherRoot := filepath.Join(t.TempDir(), "another-root.pem")
	require.NoError(t, os.WriteFile(otherRoot, anotherRoot(t), 0o600))
	client := h.start(trusting(otherRoot))

	require.Never(t, func() bool { return len(h.after(time.Time{})) > 0 }, 10*time.Second, tick,
		"T-2: no request is made to a server the configured root does not verify")

	client.stop()
	// A registration with no answer is not sent again with the same token (REG-7), so the control
	// is given a new one, as an operator would.
	minted := h.fix("mint-token", reference.FixtureRequest{Cluster: h.record})
	h.writeToken(minted["registrationToken"].(string))
	h.start()
	h.connected()
}

// anotherRoot is a root certificate of its own, PEM, that verifies no server the suite runs.
func anotherRoot(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// withUnknownMembers adds members no release of this version names to every JSON object answered.
func withUnknownMembers(w http.ResponseWriter, r *http.Request, _ []byte, next http.Handler) bool {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	var answered map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &answered) != nil {
		return false
	}
	answered["futureMember"] = map[string]any{"nested": true}
	if changes, ok := answered["changes"].([]any); ok {
		for _, c := range changes {
			if change, ok := c.(map[string]any); ok {
				change["futureMember"] = 1
			}
		}
	}
	encoded, _ := json.Marshal(answered)
	maps.Copy(w.Header(), rec.Header())
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.Code)
	_, _ = w.Write(encoded)
	return true
}

func ignoresUnknownMembers(t *testing.T) {
	h := newHarness(t)
	h.setIntercept(withUnknownMembers)
	h.declare("", bot(h.watched[0], "a", "reviewer"))

	h.start()

	h.applied(h.watched[0], "a")
	assert.NotEmpty(t, h.sent(tokenPath), "control: the exchange answer carried the unknown member too")
}

// refusedFirst answers the first n declarations requests with status, Retry-After where it is set.
func refusedFirst(n int32, status int, retryAfter string) intercept {
	var count atomic.Int32
	return func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != declarationsPath || count.Add(1) > n {
			return false
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		answer(w, status, "answered_by_the_suite")
		return true
	}
}

func backsOff(t *testing.T) {
	h := newHarness(t)
	h.setIntercept(refusedFirst(4, http.StatusServiceUnavailable, ""))

	h.start()

	require.Eventually(t, func() bool { return len(h.sent(declarationsPath)) >= 5 }, time.Minute, tick)
	waited := gaps(h.sent(declarationsPath)[:5])
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		assert.GreaterOrEqual(t, waited[i], want*95/100, "T-6: at least 1 s, doubling: %v", waited)
	}
}

func honoursRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.setIntercept(refusedFirst(1, http.StatusTooManyRequests, "4"))

	h.start()

	require.Eventually(t, func() bool { return len(h.sent(declarationsPath)) >= 2 }, eventually, tick)
	assert.GreaterOrEqual(t, gaps(h.sent(declarationsPath)[:2])[0], 4*time.Second, "T-6: Retry-After waited")
}

func unknownErrorCodeByStatus(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		h.connected()
		exchanges := len(h.sent(tokenPath))
		h.setIntercept(once(declarationsPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
			answer(w, http.StatusUnauthorized, "a_code_from_a_later_release")
		}))
		h.wake()

		require.Eventually(t, func() bool { return len(h.sent(tokenPath)) > exchanges }, eventually, tick,
			"T-5: an unknown code on a 401 is a 401, so the client exchanges again (AUTH-3)")
	})
	t.Run("503", func(t *testing.T) {
		h := newHarness(t)
		h.setIntercept(refusedFirst(2, http.StatusServiceUnavailable, ""))
		h.start()

		require.Eventually(t, func() bool { return len(h.sent(declarationsPath)) >= 3 }, eventually, tick)
		waited := gaps(h.sent(declarationsPath)[:3])
		assert.GreaterOrEqual(t, waited[0], 950*time.Millisecond, "T-5: an unknown code on a 503 is backed off as a 5xx")
		assert.GreaterOrEqual(t, waited[1], 1900*time.Millisecond)
	})
}

// neverListens reads the client process's own sockets from /proc and requires none of them to be
// listening: TCP in LISTEN, or a Unix socket accepting connections. The client named here has no probe
// port, so none is allowed.
func neverListens(t *testing.T) {
	if runtime.GOOS != "linux" {
		suite.Pending(t, "the client's sockets are read from /proc, which only Linux has")
	}
	h := newHarness(t)
	client := h.start()
	h.connected()
	h.declare("", bot(h.watched[0], "a", "reviewer"))
	h.applied(h.watched[0], "a")

	inodes := socketInodes(t, client.pid())
	require.NotEmpty(t, inodes, "control: the client's sockets are read")
	listening := listeningSockets(t, client.pid(), inodes)

	assert.Empty(t, listening, "T-1: the client opens no listening socket")
}

func socketInodes(t *testing.T, pid int) map[string]bool {
	t.Helper()
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if inode, ok := strings.CutPrefix(link, "socket:["); err == nil && ok {
			out[strings.TrimSuffix(inode, "]")] = true
		}
	}
	return out
}

func listeningSockets(t *testing.T, pid int, inodes map[string]bool) []string {
	t.Helper()
	var out []string
	for _, table := range []string{"tcp", "tcp6"} {
		for _, fields := range procTable(t, pid, table) {
			// sl local_address rem_address st ... inode at index 9; st 0A is LISTEN.
			if len(fields) > 9 && fields[3] == "0A" && inodes[fields[9]] {
				out = append(out, table+" "+fields[1])
			}
		}
	}
	for _, fields := range procTable(t, pid, "unix") {
		// Num RefCount Protocol Flags Type St Inode Path; flag 0x10000 is __SO_ACCEPTCON.
		if len(fields) > 6 && inodes[fields[6]] {
			if flags, err := strconv.ParseUint(fields[3], 16, 64); err == nil && flags&0x10000 != 0 {
				out = append(out, "unix "+strings.Join(fields[7:], " "))
			}
		}
	}
	return out
}

func procTable(t *testing.T, pid int, table string) [][]string {
	t.Helper()
	file, err := os.Open(fmt.Sprintf("/proc/%d/net/%s", pid, table))
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	var out [][]string
	scanner := bufio.NewScanner(file)
	scanner.Scan()
	for scanner.Scan() {
		out = append(out, strings.Fields(scanner.Text()))
	}
	return out
}
