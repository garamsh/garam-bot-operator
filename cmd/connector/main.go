// Command connector connects a user's cluster to garam: it registers the cluster once with a registration
// token, keeps the key it generated in one Secret, and rotates that key itself (ADR 0088, ADR 0090).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity/secret"
)

// version is the connector's version, sent at registration; a release build sets it with -ldflags -X.
var version = "dev"

// requestTimeout bounds each registration, exchange and rotation request.
const requestTimeout = 30 * time.Second

type options struct {
	garamURL         string
	rootFile         string
	tokenFile        string
	releaseNamespace string
	identitySecret   string
}

func main() {
	var o options
	flag.StringVar(&o.garamURL, "garam-url", "",
		"The cluster-connection base URL garam serves, without /cluster-connection/v1alpha1.")
	flag.StringVar(&o.rootFile, "garam-root-file", "",
		"PEM file holding the root garam's certificate chains to. Nothing an answer carries is trusted (T-2).")
	flag.StringVar(&o.tokenFile, "registration-token-file", "",
		"File holding the registration token, read only while the cluster is not registered. It is never logged.")
	flag.StringVar(&o.releaseNamespace, "release-namespace", "", "The namespace this release is installed in.")
	flag.StringVar(&o.identitySecret, "identity-secret", "garam-connector-identity",
		"The Secret in the release namespace that holds the cluster's key and record.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("connector stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, log *slog.Logger) error {
	if o.garamURL == "" || o.rootFile == "" || o.tokenFile == "" || o.releaseNamespace == "" {
		return errors.New(
			"--garam-url, --garam-root-file, --registration-token-file and --release-namespace are all required")
	}
	garam, err := garamClient(o.rootFile)
	if err != nil {
		return err
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("load the Kubernetes client configuration: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build the Kubernetes client: %w", err)
	}
	identity := clusteridentity.NewService(clusteridentity.Config{
		BaseURL:   o.garamURL,
		HTTP:      garam,
		TokenFile: o.tokenFile,
		Version:   version,
		Store:     secret.Store{Client: kube, Namespace: o.releaseNamespace, Name: o.identitySecret},
		Log:       log,
	})
	connection, err := identity.Establish(ctx)
	if err != nil {
		return err
	}
	log.Info("connected", "cluster", connection.Cluster())
	return connection.Run(ctx)
}

// garamClient verifies garam against the configured root alone, and presents nothing (T-2).
func garamClient(rootFile string) (*http.Client, error) {
	root, err := os.ReadFile(rootFile)
	if err != nil {
		return nil, fmt.Errorf("read the garam root file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		return nil, fmt.Errorf("%s holds no PEM certificate", rootFile)
	}
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}},
	}, nil
}
