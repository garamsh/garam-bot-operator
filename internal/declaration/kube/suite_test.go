package kube_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"
)

// The adapter's writes are server-side apply, preconditions and managed fields, which a fake client
// does not run, so it is tested against envtest's API server serving interface 1's definitions.
var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

func TestKube(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Declaration Objects Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	testEnv = &envtest.Environment{CRDs: definitions()}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}
	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(botv1.AddToScheme(scheme)).To(Succeed())
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	cancel()
	Eventually(func() error { return testEnv.Stop() }, time.Minute, time.Second).Should(Succeed())
})

// definitions are interface 1's CustomResourceDefinitions, as the chart installs them.
func definitions() []*apiextv1.CustomResourceDefinition {
	var out []*apiextv1.CustomResourceDefinition
	Expect(fs.WalkDir(botv1.CRDs, ".", func(path string, entry fs.DirEntry, err error) error {
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
	})).To(Succeed())
	Expect(out).To(HaveLen(4))
	return out
}

// firstEnvTestBinaryDir finds the binaries `make setup-envtest` downloads, so
// the suite runs from an IDE without KUBEBUILDER_ASSETS set.
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
