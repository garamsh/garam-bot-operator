package declaration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
	"github.com/garamsh/garam-agent-operator/internal/declaration"
)

const (
	declPath = "/cluster-connection/v1alpha1/clusters/self/declarations"
	// version is the connector version every test runs as; poolA the declaration most tests place, and
	// team a label it carries.
	version    = "test"
	poolA      = "pool-a"
	team       = "team"
	reportPath = "/cluster-connection/v1alpha1/clusters/self/reports"
	// release is the release every test's claims name, and watched and other the namespaces it watches.
	release = "garam/connector"
	watched = "bots"
	other   = "more-bots"
	// eventually is how long a test waits for a pass to show.
	eventually = 10 * time.Second
	tick       = 10 * time.Millisecond
)

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

// harness is the reference server behind an interceptor, a record registered at it, and a cluster in
// memory whose namespaces watched and other are claimed for release.
type harness struct {
	t       *testing.T
	base    string
	fixture http.Handler
	record  string
	client  *http.Client
	objects *memObjects

	mu        sync.Mutex
	intercept func(w http.ResponseWriter, r *http.Request, next http.Handler) bool
	polls     []string
	// reports are the bodies of every report sent, in order.
	reports []map[string]any
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, objects: newMemObjects()}
	var server http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if r.URL.Path == declPath {
			h.polls = append(h.polls, r.URL.RawQuery)
		}
		if r.URL.Path == reportPath {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var sent map[string]any
			_ = json.Unmarshal(body, &sent)
			h.reports = append(h.reports, sent)
		}
		intercept := h.intercept
		h.mu.Unlock()
		if intercept != nil && intercept(w, r, server) {
			return
		}
		server.ServeHTTP(w, r)
	}))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	ref := reference.New(reference.Config{BaseURL: ts.URL, PageSize: 100, Now: time.Now, Random: rand.Reader})
	server, h.base, h.fixture = ref.Handler(), ts.URL, ref.Fixture()

	got := h.fix("mint-token", reference.FixtureRequest{})
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(got["registrationToken"].(string)), 0o600))
	key := testKey()
	connection, err := clusteridentity.NewService(clusteridentity.Config{
		BaseURL: ts.URL, HTTP: ts.Client(), TokenFile: tokenFile, Version: version, Store: &identityStore{},
		GenerateKey: func() (*rsa.PrivateKey, error) { return key, nil },
	}).Establish(context.Background())
	require.NoError(t, err)
	h.record, h.client = string(connection.Cluster()), connection.Client()
	h.objects.claim(watched, release)
	h.objects.claim(other, release)
	return h
}

func (h *harness) fix(action string, q reference.FixtureRequest) map[string]any {
	h.t.Helper()
	encoded, err := json.Marshal(q)
	require.NoError(h.t, err)
	rec := httptest.NewRecorder()
	h.fixture.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+action, bytes.NewReader(encoded)))
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// declare places object on the record as declaration id's next generation, and answers that generation.
func (h *harness) declare(id string, object map[string]any) int64 {
	h.t.Helper()
	encoded, err := json.Marshal(object)
	require.NoError(h.t, err)
	got := h.fix("declare", reference.FixtureRequest{Cluster: reference.GRN(h.record), ID: id, Object: encoded})
	generation, ok := got["generation"].(float64)
	require.True(h.t, ok)
	return int64(generation)
}

func (h *harness) tombstone(id string) {
	h.t.Helper()
	h.fix("tombstone", reference.FixtureRequest{ID: id})
}

func (h *harness) setIntercept(f func(w http.ResponseWriter, r *http.Request, next http.Handler) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.intercept = f
}

func (h *harness) reportsSent() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any{}, h.reports...)
}

// lastReport waits until a report satisfying ok was sent, and answers the last one sent.
func (h *harness) lastReport(ok func(map[string]any) bool) map[string]any {
	h.t.Helper()
	var last map[string]any
	require.Eventually(h.t, func() bool {
		sent := h.reportsSent()
		if len(sent) == 0 {
			return false
		}
		last = sent[len(sent)-1]
		return ok(last)
	}, eventually, tick)
	return last
}

func (h *harness) pollsSent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.polls...)
}

// config is a Service's Config over the harness, with every namespace the tests use watched.
func (h *harness) config() declaration.Config {
	return declaration.Config{
		BaseURL:    h.base,
		HTTP:       h.client,
		Record:     h.record,
		Release:    release,
		Namespaces: []string{watched, other},
		Objects:    h.objects,
		Version:    version,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// run runs a Service under cfg until the test ends.
func (h *harness) run(cfg declaration.Config) declaration.Service {
	h.t.Helper()
	service := declaration.NewService(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			h.t.Errorf("Run ended with %v, not with its context", err)
		}
	})
	return service
}

// settled waits until the last pass's Status satisfies ok, and answers it.
func (h *harness) settled(service declaration.Service, ok func(declaration.Status) bool) declaration.Status {
	h.t.Helper()
	var status declaration.Status
	require.Eventually(h.t, func() bool {
		var ready bool
		status, ready = service.Status()
		return ready && ok(status)
	}, eventually, tick)
	return status
}

// outcome answers the Result for id in status.
func outcome(status declaration.Status, id string) (declaration.Result, bool) {
	for _, r := range status.Declarations {
		if r.ID == id {
			return r, true
		}
	}
	return declaration.Result{}, false
}

func hasOutcome(id string, want declaration.Outcome) func(declaration.Status) bool {
	return func(status declaration.Status) bool {
		got, ok := outcome(status, id)
		return ok && got.Outcome == want
	}
}

// kindPool is the kind most tests declare, and maxField its one field.
const (
	kindPool = "BotPool"
	maxField = "maxBotsPerPod"
)

// declaredObject is a declaration's object: kind at apiVersion, in namespace where it is not empty.
func declaredObject(apiVersion, kind, namespace, name string, spec map[string]any) map[string]any {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata, "spec": spec}
}

// pool is a BotPool object in namespace, as a declaration carries it.
func pool(namespace, name string, maxBotsPerPod int) map[string]any {
	return declaredObject(declaration.APIVersion, kindPool, namespace, name, map[string]any{maxField: maxBotsPerPod})
}

// memObjects is declaration.Objects in memory, refusing writes as an API server does: by name, uid and
// resourceVersion. Every change signals Changed, as an informer does.
type memObjects struct {
	mu      sync.Mutex
	objects map[declaration.Ref]*unstructured.Unstructured
	claims  map[string]string
	version int
	// calls are every write asked for, and writes those that changed something.
	calls   []string
	writes  []string
	changed chan struct{}
	// refuse, where set, answers an error for a write, which then changes nothing.
	refuse func(op string, object *unstructured.Unstructured) error
}

func newMemObjects() *memObjects {
	return &memObjects{objects: map[declaration.Ref]*unstructured.Unstructured{}, claims: map[string]string{},
		changed: make(chan struct{}, 1)}
}

func refOf(object *unstructured.Unstructured) declaration.Ref {
	return declaration.Ref{Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()}
}

func (m *memObjects) signal() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func (m *memObjects) Changed() <-chan struct{} { return m.changed }

func (m *memObjects) claim(namespace, holder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims[namespace] = holder
	m.signal()
}

func (m *memObjects) unclaim(namespace string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.claims, namespace)
	m.signal()
}

func (m *memObjects) Claim(_ context.Context, namespace string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	holder, ok := m.claims[namespace]
	return holder, ok, nil
}

func (m *memObjects) List(_ context.Context, namespace string) ([]declaration.Held, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []declaration.Held
	for ref, object := range m.objects {
		if ref.Namespace != namespace {
			continue
		}
		held := declaration.Held{Ref: ref, UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion(),
			Generation: object.GetGeneration()}
		if spec, ok := object.Object["spec"].(map[string]any); ok {
			held.Spec = runtime.DeepCopyJSON(spec)
		}
		if status, ok := object.Object["status"].(map[string]any); ok {
			held.Status = runtime.DeepCopyJSON(status)
		}
		held.Marker, held.MarkerErr = botv1.ManagementOf(object)
		out = append(out, held)
	}
	return out, nil
}

// put stores object as another writer does: a person, GitOps, or another record's connector.
func (m *memObjects) put(object *unstructured.Unstructured) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store(object.DeepCopy())
}

func (m *memObjects) store(object *unstructured.Unstructured) {
	m.version++
	ref := refOf(object)
	// As an API server keeps them: the uid for the object's life, and a generation for each spec.
	if existing, ok := m.objects[ref]; ok {
		object.SetUID(existing.GetUID())
		object.SetGeneration(existing.GetGeneration())
		if !reflect.DeepEqual(existing.Object["spec"], object.Object["spec"]) {
			object.SetGeneration(existing.GetGeneration() + 1)
		}
	} else {
		object.SetUID(types.UID(fmt.Sprintf("uid-%d", m.version)))
		object.SetGeneration(1)
	}
	object.SetResourceVersion(strconv.Itoa(m.version))
	m.objects[ref] = object
	m.signal()
}

func (m *memObjects) Create(_ context.Context, object *unstructured.Unstructured) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refused("create", object); err != nil {
		return err
	}
	if _, ok := m.objects[refOf(object)]; ok {
		return declaration.ErrNameTaken
	}
	m.calls = append(m.calls, "create "+refOf(object).String())
	m.writes = append(m.writes, "create "+refOf(object).String())
	m.store(object.DeepCopy())
	return nil
}

func (m *memObjects) Apply(_ context.Context, object *unstructured.Unstructured, held declaration.Held) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refused("apply", object); err != nil {
		return err
	}
	live, ok := m.objects[held.Ref]
	if !ok || string(live.GetUID()) != held.UID || live.GetResourceVersion() != held.ResourceVersion {
		return declaration.ErrChanged
	}
	m.calls = append(m.calls, "apply "+held.Ref.String())
	// An apply that changes nothing writes nothing, as server-side apply does.
	if reflect.DeepEqual(live.Object["spec"], object.Object["spec"]) &&
		reflect.DeepEqual(live.GetLabels(), object.GetLabels()) &&
		reflect.DeepEqual(live.GetAnnotations(), object.GetAnnotations()) {
		return nil
	}
	m.writes = append(m.writes, "apply "+held.Ref.String())
	m.store(object.DeepCopy())
	return nil
}

func (m *memObjects) Delete(_ context.Context, held declaration.Held) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	live, ok := m.objects[held.Ref]
	if !ok || string(live.GetUID()) != held.UID || live.GetResourceVersion() != held.ResourceVersion {
		return declaration.ErrChanged
	}
	m.writes = append(m.writes, "delete "+held.Ref.String())
	delete(m.objects, held.Ref)
	m.signal()
	return nil
}

func (m *memObjects) refused(op string, object *unstructured.Unstructured) error {
	if m.refuse == nil {
		return nil
	}
	return m.refuse(op, object)
}

func (m *memObjects) get(ref declaration.Ref) (*unstructured.Unstructured, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[ref]
	if !ok {
		return nil, false
	}
	return object.DeepCopy(), true
}

func (m *memObjects) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.calls...)
}

func (m *memObjects) written() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.writes...)
}

// object is a BotPool in namespace as another writer holds it, carrying labels.
func object(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{maxField: int64(1)}}}
	o.SetAPIVersion(declaration.APIVersion)
	o.SetKind(kindPool)
	o.SetNamespace(namespace)
	o.SetName(name)
	o.SetLabels(labels)
	return o
}

// garamOwned is a garam-owned BotPool of record for declaration id at generation.
func garamOwned(namespace, name, record, id string, generation int64) *unstructured.Unstructured {
	o := object(namespace, name, nil)
	if err := botv1.MarkGaramOwned(o, botv1.GaramDeclaration{Cluster: record, ID: id, Generation: generation}); err != nil {
		panic(err)
	}
	return o
}

// testKey is one RSA 3072 key for every test's record, generated once.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		panic(err)
	}
	return k
})
