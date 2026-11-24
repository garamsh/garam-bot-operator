package kube_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
	"github.com/garamsh/garam-agent-operator/internal/declaration"
)

const reportPath = "/cluster-connection/v1alpha1/clusters/self/reports"

// identityStore is a clusteridentity.KeyStore in memory.
type identityStore struct {
	mu    sync.Mutex
	state *clusteridentity.State
}

func (m *identityStore) Load(context.Context) (clusteridentity.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return clusteridentity.State{}, clusteridentity.ErrNoState
	}
	return *m.state, nil
}

func (m *identityStore) Save(_ context.Context, state clusteridentity.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = &state
	return nil
}

// TestReports_CarryNoSecretOrConfigMapWhateverTheNamespaceHolds runs the declarations against envtest
// and the reference server, in a watched namespace holding a Secret and a ConfigMap beside a Bot that
// names the Secret (REP-3, ADR 0091 decision 5).
func TestReports_CarryNoSecretOrConfigMapWhateverTheNamespaceHolds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns := namespace(t)
	const secretValue, configValue = "s3cr3t-value-never-reported", "config-value-never-reported"
	require.NoError(t, k8sClient.Create(ctx, &corev1.Secret{Namespace: ns, Name: "creds",
		StringData: map[string]string{"token": secretValue}}))
	require.NoError(t, k8sClient.Create(ctx, &corev1.ConfigMap{Namespace: ns, Name: "settings",
		Data: map[string]string{"setting": configValue}}))
	require.NoError(t, k8sClient.Create(ctx, &coordinationv1.Lease{
		Namespace: ns, Name: declaration.ClaimName, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}))
	made := &botv1.Bot{Namespace: ns, Name: "made-here", Spec: botv1.BotSpec{
		Persona: botv1.Persona{Role: "reviewer"}, Agent: botv1.Agent{Harness: "claude-code"},
		Permissions: botv1.Permissions{Secrets: []botv1.SecretKeyRef{"creds/token"}},
	}}
	require.NoError(t, k8sClient.Create(ctx, made))
	const statusMessage = "status-written-through-the-subresource"
	made.Status = botv1.BotStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: "Ready",
		Status: metav1.ConditionTrue, Reason: "Tested", Message: statusMessage, LastTransitionTime: metav1.Now()}}}
	require.NoError(t, k8sClient.Status().Update(ctx, made))

	var mu sync.Mutex
	var reports [][]byte
	var server http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == reportPath {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			mu.Lock()
			reports = append(reports, body)
			mu.Unlock()
		}
		server.ServeHTTP(w, r)
	}))
	ts.StartTLS()
	defer ts.Close()
	ref := reference.New(reference.Config{BaseURL: ts.URL, PageSize: 100, Now: time.Now, Random: rand.Reader})
	server = ref.Handler()
	connection := register(t, ts, ref.Fixture())

	objects := started(t, cfg, ns)
	service := declaration.NewService(declaration.Config{
		BaseURL: ts.URL, HTTP: connection.Client(), Record: string(connection.Cluster()), Release: holder,
		Namespaces: []string{ns}, Version: "test", Objects: objects,
	})
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()

	var last []byte
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(reports) == 0 {
			return false
		}
		last = reports[len(reports)-1]
		return bytes.Contains(last, []byte(`"made-here"`)) && bytes.Contains(last, []byte(statusMessage))
	}, eventually, tick, "REP-2: a report carrying the cluster-owned Bot with its status")

	var report struct {
		Objects []struct {
			Kind string `json:"kind"`
		} `json:"objects"`
	}
	require.NoError(t, json.Unmarshal(last, &report))
	for _, o := range report.Objects {
		assert.Contains(t, declaration.Kinds, o.Kind, "REP-3: only declared kinds are reported")
	}
	assert.NotContains(t, string(last), secretValue)
	assert.NotContains(t, string(last), configValue)
	assert.NotContains(t, string(last), `"settings"`, "the ConfigMap is not reported")
	assert.Contains(t, string(last), `"creds/token"`, "control: the Bot's spec names the Secret by reference alone")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// register registers a record at the reference server, and answers its Connection.
func register(t *testing.T, ts *httptest.Server, fixture http.Handler) clusteridentity.Connection {
	t.Helper()
	rec := httptest.NewRecorder()
	fixture.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mint-token", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var minted struct {
		RegistrationToken string `json:"registrationToken"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &minted))
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(minted.RegistrationToken), 0o600))
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	require.NoError(t, err)
	connection, err := clusteridentity.NewService(clusteridentity.Config{
		BaseURL: ts.URL, HTTP: ts.Client(), TokenFile: tokenFile, Version: "test", Store: &identityStore{},
		GenerateKey: func() (*rsa.PrivateKey, error) { return key, nil },
	}).Establish(context.Background())
	require.NoError(t, err)
	return connection
}
