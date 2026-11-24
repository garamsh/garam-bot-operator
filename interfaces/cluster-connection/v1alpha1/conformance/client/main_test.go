//go:build conformance

// Package client is conformance.md's client suite. It starts the client under test from the command
// line CONFORMANCE_CLIENT names, against a reference server of its own per test and an envtest API
// server carrying interface 1's CRDs, and reaches the client only through the protocol and the cluster.
package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/suite"
)

const prefix = "client/"

// The environment a client suite run is given.
const (
	// EnvClient is the client's command line, with conformance.md's placeholders in braces.
	EnvClient = "CONFORMANCE_CLIENT"
	// EnvClientSecrets names the empty Secrets the client's installer creates in its release namespace
	// before the client starts, comma-separated.
	EnvClientSecrets = "CONFORMANCE_CLIENT_SECRETS"
)

var (
	// kube is the API server's admin client, and kubeconfig the file the client is given for it.
	kube       client.Client
	kubeconfig string
	command    []string
	secrets    []string
	setupErr   error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	command = strings.Fields(os.Getenv(EnvClient))
	for name := range strings.SplitSeq(os.Getenv(EnvClientSecrets), ",") {
		if name = strings.TrimSpace(name); name != "" {
			secrets = append(secrets, name)
		}
	}
	if len(command) == 0 {
		setupErr = fmt.Errorf("%s names the client under test, and is not set", EnvClient)
		return suite.Main(m)
	}
	env := &envtest.Environment{}
	definitions, err := crds()
	if err != nil {
		setupErr = err
		return suite.Main(m)
	}
	env.CRDs = definitions
	cfg, err := env.Start()
	if err != nil {
		setupErr = fmt.Errorf("start envtest, whose binaries KUBEBUILDER_ASSETS names: %w", err)
		return suite.Main(m)
	}
	defer func() { _ = env.Stop() }()
	if setupErr = connect(env, cfg); setupErr != nil {
		return suite.Main(m)
	}
	defer func() { _ = os.Remove(kubeconfig) }()
	return suite.Main(m)
}

// connect builds the suite's admin client and writes the kubeconfig the client under test is given.
func connect(env *envtest.Environment, cfg *rest.Config) error {
	var err error
	if kube, err = client.New(cfg, client.Options{}); err != nil {
		return fmt.Errorf("build the API server's client: %w", err)
	}
	user, err := env.AddUser(envtest.User{Name: "client-under-test", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return fmt.Errorf("add the client's user: %w", err)
	}
	config, err := user.KubeConfig()
	if err != nil {
		return fmt.Errorf("write the client's kubeconfig: %w", err)
	}
	file, err := os.CreateTemp("", "conformance-kubeconfig-*")
	if err != nil {
		return err
	}
	kubeconfig = file.Name()
	_, err = file.Write(config)
	return errors.Join(err, file.Close())
}

// crds are interface 1's CustomResourceDefinitions, as garam-bot's api module carries them.
func crds() ([]*apiextensionsv1.CustomResourceDefinition, error) {
	var out []*apiextensionsv1.CustomResourceDefinition
	err := fs.WalkDir(botv1.CRDs, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		read, err := fs.ReadFile(botv1.CRDs, path)
		if err != nil {
			return err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(read, crd); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, crd)
		return nil
	})
	return out, err
}

// tests are the suite. Each runs in parallel with the others, under a reference server, namespaces and
// a client process of its own, so client/reports/at-least-every-5m's five minutes overlap the rest.
var tests = []suite.Test{
	{Name: "client/registration/generates-key-sends-public-half", Run: generatesKeySendsPublicHalf},
	{Name: "client/registration/no-retry-after-refusal", Run: noRetryAfterRefusal},
	{Name: "client/registration/no-retry-after-no-answer", Run: noRetryAfterNoAnswer},
	{Name: "client/registration/retries-after-429", Run: retriesAfter429},
	{Name: "client/transport/verifies-configured-root", Run: verifiesConfiguredRoot},
	{Name: "client/transport/ignores-unknown-members", Run: ignoresUnknownMembers},
	{Name: "client/transport/backs-off", Run: backsOff},
	{Name: "client/transport/honours-retry-after", Run: honoursRetryAfter},
	{Name: "client/transport/unknown-error-code-by-status", Run: unknownErrorCodeByStatus},
	{Name: "client/transport/never-listens", Run: neverListens},
	{Name: "client/auth/re-exchanges-once-on-401", Run: reExchangesOnceOn401},
	{Name: "client/rotation/signs-authorization-with-current-key", Run: signsAuthorizationWithCurrentKey},
	{Name: "client/rotation/stores-successor-before-sending", Run: storesSuccessorBeforeSending},
	{Name: "client/rotation/signs-with-successor-after-answer", Run: signsWithSuccessorAfterAnswer},
	{Name: "client/rotation/tries-successor-after-lost-answer", Run: triesSuccessorAfterLostAnswer},
	{Name: "client/rotation/tries-successor-after-409", Run: triesSuccessorAfter409},
	{Name: "client/rotation/repeat-after-lost-answer-uses-fresh-jti", Run: repeatAfterLostAnswerUsesFreshJTI},
	{Name: "client/rotation/keeps-predecessor-after-409-and-failed-exchange", Run: keepsPredecessorAfter409},
	{Name: "client/declarations/applies-full-set", Run: appliesFullSet},
	{Name: "client/declarations/waits-for-more-false", Run: waitsForMoreFalse},
	{Name: "client/declarations/rereads-on-410", Run: rereadsOn410},
	{Name: "client/declarations/ignores-lower-generation", Run: ignoresLowerGeneration},
	{Name: "client/declarations/marks-own-record", Run: marksOwnRecord},
	{Name: "client/declarations/refuses-name-held-by-cluster", Run: refusesNameHeldByCluster},
	{Name: "client/declarations/refuses-name-held-by-another-record", Run: refusesNameHeldByAnotherRecord},
	{Name: "client/declarations/refuses-invalid", Run: refusesInvalid},
	{Name: "client/declarations/reports-unwatched-namespace", Run: reportsUnwatchedNamespace},
	{Name: "client/declarations/deletes-only-on-tombstone", Run: deletesOnlyOnTombstone},
	{Name: "client/declarations/reports-orphan", Run: reportsOrphan},
	{Name: "client/declarations/leaves-cluster-owned-alone", Run: leavesClusterOwnedAlone},
	{Name: "client/declarations/report-only-applies-nothing", Run: reportOnlyAppliesNothing},
	{Name: "client/claims/acts-only-under-own-claim", Run: actsOnlyUnderOwnClaim},
	{Name: "client/claims/never-writes-a-claim", Run: neverWritesAClaim},
	{Name: "client/claims/stays-out-of-namespace-claimed-elsewhere", Run: staysOutOfNamespaceClaimedElsewhere},
	{Name: "client/claims/stays-out-of-unclaimed-namespace", Run: staysOutOfUnclaimedNamespace},
	{Name: "client/reports/whole-view", Run: wholeView},
	{Name: "client/reports/leaves-out-other-records-objects", Run: leavesOutOtherRecordsObjects},
	{Name: "client/reports/no-secret-or-configmap", Run: noSecretOrConfigMap},
	{Name: "client/reports/within-10s-of-change", Run: within10sOfChange},
	{Name: "client/reports/at-least-every-5m", Run: atLeastEvery5m},
	{Name: "client/reports/goes-on-after-413", Run: goesOnAfter413},
	{Name: "client/reports/backs-off-on-5xx", Run: reportsBackOffOn5xx},
	{Name: "client/reports/waits-retry-after", Run: reportsWaitRetryAfter},
	{Name: "client/reports/after-restart", Run: reportsAfterRestart},
}

// TestClient runs the suite under conformance.md's names.
func TestClient(t *testing.T) {
	require.NoError(t, setupErr, "the client under test and its API server")
	parallel := make([]suite.Test, 0, len(tests))
	for _, test := range tests {
		parallel = append(parallel, suite.Test{Name: test.Name, Run: func(t *testing.T) {
			t.Parallel()
			test.Run(t)
		}})
	}
	suite.Run(t, prefix, parallel)
}

// TestClient_NamesEveryTestOfConformanceMd holds the suite to conformance.md: each test it names exists,
// under its name, and no other.
func TestClient_NamesEveryTestOfConformanceMd(t *testing.T) {
	named, err := suite.Names("../../conformance.md", prefix)
	require.NoError(t, err)
	require.NotEmpty(t, named, "control: conformance.md is read")
	built := make([]string, 0, len(tests))
	for _, test := range tests {
		built = append(built, test.Name)
	}
	slices.Sort(named)
	slices.Sort(built)
	assert.Equal(t, named, built)
}
