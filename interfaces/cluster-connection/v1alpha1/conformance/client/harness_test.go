//go:build conformance

package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/suite"
	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// The routes of README.md, relative to the base URL, and the reference server's tokenUrl.
const (
	registrationsPath = "/cluster-connection/v1alpha1/registrations"
	keysPath          = "/cluster-connection/v1alpha1/clusters/self/keys"
	declarationsPath  = "/cluster-connection/v1alpha1/clusters/self/declarations"
	reportsPath       = "/cluster-connection/v1alpha1/clusters/self/reports"
	tokenPath         = "/oauth/token"
	// claimName is CLAIM-1's Lease.
	claimName = "garam-connector-claim"
	release   = "connector"
)

const (
	eventually = 30 * time.Second
	tick       = 100 * time.Millisecond
)

var serial atomic.Int64

// seen is one request the server was sent.
type seen struct {
	at     time.Time
	method string
	path   string
	query  string
	bearer string
	body   []byte
}

// intercept answers a request in place of the server, or calls next itself, and answers whether it did.
type intercept func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) bool

// harness is one test's world: a reference server behind a proxy that records and can answer every
// request, a record with a registration token minted for it, a release namespace and watched
// namespaces whose claims name the release, and the client under test.
type harness struct {
	t         *testing.T
	ref       *reference.Server
	base      string
	trustFile string
	record    reference.GRN
	tokenFile string
	releaseNS string
	watched   []string

	mu        sync.Mutex
	requests  []seen
	intercept intercept

	process *process
}

type options struct {
	pageSize int
	// unclaimed watched namespaces get no claim; elsewhere ones a claim naming another release.
	claimed, unclaimed, elsewhere int
}

func newHarness(t *testing.T, opts ...func(*options)) *harness {
	t.Helper()
	o := options{pageSize: 100, claimed: 1}
	for _, set := range opts {
		set(&o)
	}
	h := &harness{t: t}
	var handler http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, handler)
	}))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	h.base = ts.URL
	h.ref = reference.New(reference.Config{BaseURL: ts.URL, PageSize: o.pageSize, Now: time.Now, Random: rand.Reader})
	handler = h.ref.Handler()
	h.trustFile = filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(h.trustFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600))

	minted := h.fix("mint-token", reference.FixtureRequest{})
	h.record = reference.GRN(minted["cluster"].(string))
	h.tokenFile = filepath.Join(t.TempDir(), "registration-token")
	h.writeToken(minted["registrationToken"].(string))

	n := serial.Add(1)
	h.releaseNS = fmt.Sprintf("release-%d", n)
	h.namespace(h.releaseNS)
	for _, name := range secrets {
		require.NoError(t, kube.Create(context.Background(), &corev1.Secret{
			Namespace: h.releaseNS, Name: name}))
	}
	for i := range o.claimed + o.unclaimed + o.elsewhere {
		ns := fmt.Sprintf("watched-%d-%d", n, i)
		h.namespace(ns)
		h.watched = append(h.watched, ns)
		switch {
		case i < o.claimed:
			h.claim(ns, h.releaseNS+"/"+release)
		case i >= o.claimed+o.unclaimed:
			h.claim(ns, h.releaseNS+"/another-release")
		}
	}
	return h
}

func pageSize(n int) func(*options) { return func(o *options) { o.pageSize = n } }

// namespaces adds unclaimed and elsewhere watched namespaces beside the one the release claims.
func namespaces(unclaimed, elsewhere int) func(*options) {
	return func(o *options) { o.unclaimed, o.elsewhere = unclaimed, elsewhere }
}

func (h *harness) namespace(name string) {
	h.t.Helper()
	require.NoError(h.t, kube.Create(context.Background(), &corev1.Namespace{Name: name}))
}

// claim creates CLAIM-1's Lease in namespace, naming holder, as the client's installer does.
func (h *harness) claim(namespace, holder string) {
	h.t.Helper()
	require.NoError(h.t, kube.Create(context.Background(), &coordinationv1.Lease{
		Namespace: namespace, Name: claimName,
		Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}))
}

func (h *harness) writeToken(token string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.tokenFile, []byte(token), 0o600))
}

func (h *harness) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	h.mu.Lock()
	h.requests = append(h.requests, seen{at: time.Now(), method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		bearer: bearer, body: body})
	answer := h.intercept
	h.mu.Unlock()
	if answer != nil && answer(w, r, body, next) {
		return
	}
	next.ServeHTTP(w, r)
}

func (h *harness) setIntercept(f intercept) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.intercept = f
}

// sent are the requests made to path, in the order the server was sent them.
func (h *harness) sent(path string) []seen {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []seen
	for _, s := range h.requests {
		if s.path == path {
			out = append(out, s)
		}
	}
	return out
}

// after are every request made after at, in order.
func (h *harness) after(at time.Time) []seen {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []seen
	for _, s := range h.requests {
		if s.at.After(at) {
			out = append(out, s)
		}
	}
	return out
}

func (h *harness) fix(action string, q reference.FixtureRequest) map[string]any {
	h.t.Helper()
	encoded, err := json.Marshal(q)
	require.NoError(h.t, err)
	rec := httptest.NewRecorder()
	h.ref.Fixture().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+action, bytes.NewReader(encoded)))
	require.Equal(h.t, http.StatusOK, rec.Code, "%s: %s", action, rec.Body.String())
	var out map[string]any
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// declare places object on the harness's record, as id's next generation, or as a new declaration.
func (h *harness) declare(id string, object map[string]any) string {
	h.t.Helper()
	encoded, err := json.Marshal(object)
	require.NoError(h.t, err)
	got := h.fix("declare", reference.FixtureRequest{Cluster: h.record, ID: id, Object: encoded})
	return got["id"].(string)
}

// bot is a Bot interface 1's schema admits, in namespace.
func bot(namespace, name, role string) map[string]any {
	return map[string]any{
		"apiVersion": botv1.GroupVersion.String(), "kind": "Bot",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"persona": map[string]any{"role": role}, "agent": map[string]any{"harness": "claude-code"},
		},
	}
}

// object reads a Bot from the cluster, answering whether it exists.
func (h *harness) object(namespace, name string) (*unstructured.Unstructured, bool) {
	h.t.Helper()
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(botv1.GroupVersion.String())
	o.SetKind("Bot")
	err := kube.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, o)
	if err != nil {
		return nil, false
	}
	return o, true
}

// put creates a Bot in the cluster as another writer does, marked as owner says.
func (h *harness) put(namespace, name, role string, mark func(*unstructured.Unstructured)) *unstructured.Unstructured {
	h.t.Helper()
	o := &unstructured.Unstructured{Object: bot(namespace, name, role)}
	if mark != nil {
		mark(o)
	}
	require.NoError(h.t, kube.Create(context.Background(), o))
	return o
}

func role(o *unstructured.Unstructured) string {
	r, _, _ := unstructured.NestedString(o.Object, "spec", "persona", "role")
	return r
}

// applied waits until the Bot namespace/name exists in the cluster, and answers it.
func (h *harness) applied(namespace, name string) *unstructured.Unstructured {
	h.t.Helper()
	var o *unstructured.Unstructured
	require.Eventually(h.t, func() bool {
		var ok bool
		o, ok = h.object(namespace, name)
		return ok
	}, eventually, tick, "the Bot %s/%s was applied", namespace, name)
	return o
}

// reports are the bodies of every report sent, in order.
func (h *harness) reports() []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, s := range h.sent(reportsPath) {
		var report map[string]any
		if json.Unmarshal(s.body, &report) == nil {
			out = append(out, report)
		}
	}
	return out
}

// report waits until a report satisfying ok is sent, and answers the first one that does.
func (h *harness) report(ok func(map[string]any) bool, why string) map[string]any {
	h.t.Helper()
	var found map[string]any
	require.Eventually(h.t, func() bool {
		for _, r := range h.reports() {
			if ok(r) {
				found = r
				return true
			}
		}
		return false
	}, eventually, tick, why)
	return found
}

// reportedObjects are a report's objects by name.
func reportedObjects(report map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	objects, _ := report["objects"].([]any)
	for _, o := range objects {
		object, _ := o.(map[string]any)
		name, _ := object["name"].(string)
		out[name] = object
	}
	return out
}

// outcomes are a report's declaration outcomes by identifier.
func outcomes(report map[string]any) map[string]string {
	out := map[string]string{}
	declarations, _ := report["declarations"].([]any)
	for _, d := range declarations {
		outcome, _ := d.(map[string]any)
		id, _ := outcome["id"].(string)
		out[id], _ = outcome["outcome"].(string)
	}
	return out
}

func hasOutcome(id, outcome string) func(map[string]any) bool {
	return func(r map[string]any) bool { return outcomes(r)[id] == outcome }
}

func carries(names ...string) func(map[string]any) bool {
	return func(r map[string]any) bool {
		objects := reportedObjects(r)
		for _, name := range names {
			if _, ok := objects[name]; !ok {
				return false
			}
		}
		return true
	}
}

// start options fill the command line's optional placeholders.
type start struct {
	rotationPeriod string
	reportOnly     bool
	trustFile      string
}

func rotatingEvery(period string) func(*start) { return func(s *start) { s.rotationPeriod = period } }

func reportingOnly() func(*start) { return func(s *start) { s.reportOnly = true } }

func trusting(file string) func(*start) { return func(s *start) { s.trustFile = file } }

// supports answers whether the client's command line carries placeholder, ending t as pending where it
// does not.
func supports(t *testing.T, placeholder, needs string) {
	t.Helper()
	for _, arg := range command {
		if strings.Contains(arg, "{"+placeholder+"}") {
			return
		}
	}
	suite.Pending(t, fmt.Sprintf("the client's command line has no {%s}, which %s needs", placeholder, needs))
}

// start starts the client under test, and stops it when the test ends.
func (h *harness) start(opts ...func(*start)) *process {
	h.t.Helper()
	s := start{trustFile: h.trustFile}
	for _, set := range opts {
		set(&s)
	}
	values := map[string]string{
		"base-url": h.base, "trust-file": s.trustFile, "token-file": h.tokenFile,
		"release-namespace": h.releaseNS, "release-name": release, "watch-namespaces": strings.Join(h.watched, ","),
		"rotation-period": s.rotationPeriod, "report-only": fmt.Sprint(s.reportOnly),
	}
	var args []string
	for _, arg := range command {
		filled, complete := arg, true
		for name, value := range values {
			if strings.Contains(filled, "{"+name+"}") {
				complete = complete && value != ""
				filled = strings.ReplaceAll(filled, "{"+name+"}", value)
			}
		}
		if complete {
			args = append(args, filled)
		}
	}
	p := &process{t: h.t, args: args, log: filepath.Join(h.t.TempDir(), "client.log")}
	p.run()
	h.process = p
	h.t.Cleanup(func() {
		p.stop()
		if h.t.Failed() {
			read, _ := os.ReadFile(p.log)
			h.t.Logf("the client's log:\n%s", tail(string(read), 4000))
		}
	})
	return p
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// process is the client under test, run as its command line says.
type process struct {
	t    *testing.T
	args []string
	log  string
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *process) run() {
	p.t.Helper()
	log, err := os.OpenFile(p.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(p.t, err)
	cmd := exec.Command(p.args[0], p.args[1:]...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(p.t, cmd.Start(), "start the client: %v", p.args)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(done)
	}()
	p.mu.Lock()
	p.cmd, p.done = cmd, done
	p.mu.Unlock()
}

// stop ends the client as its installer would, by SIGTERM, and kills it if it has not ended in 10 s.
func (p *process) stop() {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// kill ends the client at once, as a node lost under it would.
func (p *process) kill() {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	_ = cmd.Process.Kill()
	<-done
}

// restart starts the client again with the same command line.
func (p *process) restart() {
	p.t.Helper()
	p.run()
}

func (p *process) pid() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd.Process.Pid
}

// exited answers whether the client has ended.
func (p *process) exited() bool {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// wake places a declaration, so a poll the server holds is answered and the client asks again.
func (h *harness) wake() {
	h.t.Helper()
	h.declare("", bot(h.watched[0], fmt.Sprintf("wake-%d", serial.Add(1)), "reviewer"))
}

// connected waits until the client has registered and polled its declarations once.
func (h *harness) connected() {
	h.t.Helper()
	require.Eventually(h.t, func() bool { return len(h.sent(declarationsPath)) > 0 }, eventually, tick,
		"the client registered and asked for its declarations")
}

// answer writes T-5's error body with status.
func answer(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q,"message":"answered by the conformance suite"}`, code)
}

// drop closes the connection without an answer.
func drop(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic("the suite's server does not let a connection be dropped")
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// once answers the first request to path that also satisfies match with f, and passes every other on.
func once(path string, f func(w http.ResponseWriter, r *http.Request, next http.Handler)) intercept {
	var done atomic.Bool
	return func(w http.ResponseWriter, r *http.Request, _ []byte, next http.Handler) bool {
		if r.URL.Path != path || !done.CompareAndSwap(false, true) {
			return false
		}
		f(w, r, next)
		return true
	}
}

// gaps are the times between consecutive requests.
func gaps(requests []seen) []time.Duration {
	var out []time.Duration
	for i := 1; i < len(requests); i++ {
		out = append(out, requests[i].at.Sub(requests[i-1].at))
	}
	return out
}
