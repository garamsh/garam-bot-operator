// Command connector connects a user's cluster to garam: it registers the cluster once with a registration
// token, keeps the key it generated in one Secret, and rotates that key itself (ADR 0088, ADR 0090). It
// applies garam's declarations to the namespaces it watches, one owner per object, and reports what they
// hold (ADR 0089).
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
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity/secret"
	"github.com/garamsh/garam-agent-operator/internal/declaration"
	kubeobjects "github.com/garamsh/garam-agent-operator/internal/declaration/kube"
)

// version is the connector's version, sent at registration; a release build sets it with -ldflags -X.
var version = "dev"

// requestTimeout bounds each request to garam: the client's Timeout each exchange, and clusteridentity's
// RequestTimeout each registration and rotation, the exchange it carries included.
const requestTimeout = 30 * time.Second

type options struct {
	garamURL         string
	rootFile         string
	tokenFile        string
	releaseNamespace string
	releaseName      string
	identitySecret   string
	watchNamespaces  string
	reportOnly       bool
	rotationPeriod   time.Duration
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
	flag.StringVar(&o.releaseName, "release-name", "",
		"This release's name. A watched namespace is acted in only while its claim names <release-namespace>/<release-name>.")
	flag.StringVar(&o.watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces the connector watches, applies declarations to, and caches. Nothing else is read.")
	flag.BoolVar(&o.reportOnly, "report-only", false, "Apply and delete nothing, and still report (DECL-15).")
	flag.DurationVar(&o.rotationPeriod, "rotation-period", 30*24*time.Hour,
		"How long the cluster's key signs before the connector rotates it, more than 0 (ADR 0094).")
	flag.StringVar(&o.identitySecret, "identity-secret", "garam-connector-identity",
		"The Secret in the release namespace that holds the cluster's key and record. The chart creates it empty.")
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
	namespaces := watchedNamespaces(o.watchNamespaces)
	if o.garamURL == "" || o.rootFile == "" || o.tokenFile == "" || o.releaseNamespace == "" || o.releaseName == "" ||
		len(namespaces) == 0 {
		return errors.New("--garam-url, --garam-root-file, --registration-token-file, --release-namespace, " +
			"--release-name and --watch-namespaces are all required")
	}
	if o.rotationPeriod <= 0 {
		return fmt.Errorf("--rotation-period is %v, and must be more than 0", o.rotationPeriod)
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
		BaseURL:        o.garamURL,
		HTTP:           garam,
		TokenFile:      o.tokenFile,
		Version:        version,
		Store:          secret.New(kube, o.releaseNamespace, o.identitySecret),
		Log:            log,
		RequestTimeout: requestTimeout,
		RotationPeriod: o.rotationPeriod,
	})
	connection, err := identity.Establish(ctx)
	if err != nil {
		return err
	}
	log.Info("connected", "cluster", connection.Cluster())
	objects, err := kubeobjects.New(restConfig, namespaces)
	if err != nil {
		return err
	}
	declarations := declaration.NewService(declaration.Config{
		BaseURL:    o.garamURL,
		HTTP:       connection.Client(),
		Record:     string(connection.Cluster()),
		Release:    o.releaseNamespace + "/" + o.releaseName,
		Namespaces: namespaces,
		ReportOnly: o.reportOnly,
		Version:    version,
		Objects:    objects,
		Log:        log,
	})
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return connection.Run(ctx) })
	group.Go(func() error { return objects.Start(ctx) })
	group.Go(func() error {
		if !objects.WaitForCacheSync(ctx) {
			return ctx.Err()
		}
		return declarations.Run(ctx)
	})
	return group.Wait()
}

// watchedNamespaces reads --watch-namespaces: each name once, without blanks.
func watchedNamespaces(flagValue string) []string {
	var out []string
	for name := range strings.SplitSeq(flagValue, ",") {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// garamClient verifies garam against the configured root alone, and presents nothing (T-2). It keeps
// the default transport's proxy and timeouts, and follows no redirect: a redirect would carry the
// registration token, or the bearer token, to wherever it pointed.
func garamClient(rootFile string) (*http.Client, error) {
	root, err := os.ReadFile(rootFile)
	if err != nil {
		return nil, fmt.Errorf("read the garam root file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(root) {
		return nil, fmt.Errorf("%s holds no PEM certificate", rootFile)
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("the default HTTP transport is not an *http.Transport")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
