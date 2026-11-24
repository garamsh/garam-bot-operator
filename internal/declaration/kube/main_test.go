package kube_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/garamsh/garam-agent-operator/internal/declaration"
	"github.com/garamsh/garam-agent-operator/internal/declaration/kube"
)

// The adapter's writes are server-side apply, JSON patches under preconditions and managed fields,
// which a fake client does not run, so it is tested against envtest's API server serving interface 1's
// definitions.
var (
	cfg       *rest.Config
	k8sClient client.Client
)

const (
	// eventually is how long a test waits for the cache, and tick how often it looks.
	eventually = 10 * time.Second
	tick       = 50 * time.Millisecond
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	env := &envtest.Environment{}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	definitions, err := readDefinitions()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	env.CRDs = definitions
	if cfg, err = env.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start envtest:", err)
		return 1
	}
	defer func() { _ = env.Stop() }()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := botv1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// readDefinitions are interface 1's CustomResourceDefinitions, as the chart installs them.
func readDefinitions() ([]*apiextv1.CustomResourceDefinition, error) {
	var out []*apiextv1.CustomResourceDefinition
	err := fs.WalkDir(botv1.CRDs, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		read, err := fs.ReadFile(botv1.CRDs, path)
		if err != nil {
			return err
		}
		definition := &apiextv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(read, definition); err != nil {
			return err
		}
		out = append(out, definition)
		return nil
	})
	if err == nil && len(out) != 4 {
		err = fmt.Errorf("interface 1 carries %d definitions, not 4", len(out))
	}
	return out, err
}

var spaces atomic.Int32

// namespace creates a namespace of its own for one test.
func namespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("watched-%d", spaces.Add(1))
	require.NoError(t, k8sClient.Create(context.Background(), &corev1.Namespace{Name: name}))
	return name
}

// started is Objects over config watching namespaces, its cache started and synced until the test ends.
func started(t *testing.T, config *rest.Config, namespaces ...string) *kube.Objects {
	t.Helper()
	objects, err := kube.New(config, namespaces)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = objects.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.True(t, objects.WaitForCacheSync(ctx))
	return objects
}

// heldAs waits until the cache lists the object at name in namespace, as ok wants it, and answers it.
func heldAs(t *testing.T, objects *kube.Objects, namespace, name string, ok func(declaration.Held) bool) declaration.Held {
	t.Helper()
	var found declaration.Held
	require.Eventually(t, func() bool {
		listed, err := objects.List(context.Background(), namespace)
		if err != nil {
			return false
		}
		for _, held := range listed {
			if held.Ref.Name == name && ok(held) {
				found = held
				return true
			}
		}
		return false
	}, eventually, tick, "%s/%s is not listed as wanted", namespace, name)
	return found
}

func present(declaration.Held) bool { return true }

// atVersion wants the object as it is at version.
func atVersion(version string) func(declaration.Held) bool {
	return func(h declaration.Held) bool { return h.ResourceVersion == version }
}

// firstEnvTestBinaryDir finds the binaries `make setup-envtest` downloads, so the tests run from an IDE
// without KUBEBUILDER_ASSETS set.
func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
