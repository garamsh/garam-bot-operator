package reference

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The files serve writes into its directory, which the fixture subcommands and a client read.
const (
	// TrustFile holds the generated root, PEM, for a client to trust (T-2).
	TrustFile = "ca.pem"
	// URLFile holds the base URL the server is reached at.
	URLFile = "url"
	// FixtureSocket is the Unix socket the fixture subcommands reach the server's memory through.
	FixtureSocket = "fixture.sock"
)

const usage = `usage: cluster-connection-reference --dir DIR <subcommand> [arguments]

  serve [--listen ADDR] [--host NAME] [--page-size N] [--registration-limit N]
  mint-token [--cluster GRN] [--expired]
  declare FILE            FILE is {"cluster": GRN, "id": ID (optional), "object": DeclaredObject}
  tombstone ID
  latest-report GRN
  revoke-keys GRN
  delete-cluster GRN
  expire-cursors GRN
`

// Main runs one subcommand: serve, or a fixture subcommand printing its answer as JSON. It answers the
// process's exit status.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	global := flag.NewFlagSet("cluster-connection-reference", flag.ContinueOnError)
	global.SetOutput(stderr)
	dir := global.String("dir", "", "the directory serve writes its trust root, URL and fixture socket into")
	if err := global.Parse(args); err != nil || *dir == "" || global.NArg() == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	name, rest := global.Arg(0), global.Args()[1:]
	var err error
	if name == "serve" {
		err = runServe(ctx, *dir, rest, stdout, stderr)
	} else {
		err = runFixture(ctx, *dir, name, rest, stdout)
	}
	var usageErr usageError
	switch {
	case errors.As(err, &usageErr):
		_, _ = fmt.Fprintf(stderr, "%s\n%s", usageErr, usage)
		return 2
	case err != nil:
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// usageError is a command line Main cannot run.
type usageError string

func (e usageError) Error() string { return string(e) }

// Ready is the line serve prints once both listeners accept.
type Ready struct {
	BaseURL   string `json:"baseUrl"`
	TrustFile string `json:"trustFile"`
}

func runServe(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:0", "the address the protocol is served at")
	host := flags.String("host", "localhost",
		"the host name in the base URL; the certificate also names 127.0.0.1 and ::1")
	pageSize := flags.Int("page-size", 100, "the changes one declarations answer carries before it says more")
	limit := flags.Int("registration-limit", 30, "registration requests per source address per minute; 0 for none")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || *pageSize < 1 || *limit < 0 {
		return usageError("serve takes only its flags, with --page-size at least 1")
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	root, serving, err := generateTLS([]string{*host, "127.0.0.1", "::1"}, time.Now())
	if err != nil {
		return err
	}
	protocol, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen at %s: %w", *listen, err)
	}
	socket := filepath.Join(dir, FixtureSocket)
	fixture, err := listenFixture(socket)
	if err != nil {
		_ = protocol.Close()
		return err
	}
	port := protocol.Addr().(*net.TCPAddr).Port
	baseURL := fmt.Sprintf("https://%s", net.JoinHostPort(*host, fmt.Sprint(port)))
	trust := filepath.Join(dir, TrustFile)
	for path, content := range map[string][]byte{trust: root, filepath.Join(dir, URLFile): []byte(baseURL + "\n")} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			_ = protocol.Close()
			_ = fixture.Close()
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	server := New(Config{
		BaseURL: baseURL, PageSize: *pageSize, RegistrationLimit: *limit,
		Now: time.Now, Random: rand.Reader, Log: log,
	})
	servers := []*http.Server{
		{
			Handler:           server.Handler(),
			TLSConfig:         &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS12},
			ReadHeaderTimeout: 10 * time.Second,
		},
		{Handler: server.Fixture(), ReadHeaderTimeout: 10 * time.Second},
	}
	failed := make(chan error, 2)
	go func() { failed <- servers[0].ServeTLS(protocol, "", "") }()
	go func() { failed <- servers[1].Serve(fixture) }()
	if err := json.NewEncoder(stdout).Encode(Ready{BaseURL: baseURL, TrustFile: trust}); err != nil {
		return fmt.Errorf("print the ready line: %w", err)
	}
	log.Info("serving", "base_url", baseURL, "fixture", socket)

	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// listenFixture listens at socket, reachable by this user alone, since the fixture authenticates nothing.
// The socket is made in a directory only this user can enter, restricted, and only then renamed into
// place, so it is never reachable by another user, and the process's umask is never changed. A socket a
// previous run left there is removed; anything else there is left and refused.
func listenFixture(socket string) (net.Listener, error) {
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("remove the stale %s: %w", socket, err)
		}
	}
	private, err := os.MkdirTemp(filepath.Dir(socket), ".fixture-")
	if err != nil {
		return nil, fmt.Errorf("make a private directory beside %s: %w", socket, err)
	}
	defer func() { _ = os.RemoveAll(private) }()
	staged := filepath.Join(private, FixtureSocket)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: staged, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen at %s: %w", socket, err)
	}
	// The listener would unlink its staged path on close, which the rename below moves away.
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(staged, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict %s: %w", socket, err)
	}
	if err := os.Rename(staged, socket); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("move the socket to %s: %w", socket, err)
	}
	return listener, nil
}

// runFixture sends one fixture subcommand to the server serving dir, and prints its answer.
func runFixture(ctx context.Context, dir, name string, args []string, stdout io.Writer) error {
	request, err := fixtureRequest(name, args)
	if err != nil {
		return err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode the fixture request: %w", err)
	}
	socket := filepath.Join(dir, FixtureSocket)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/"+name, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the fixture request: %w", err)
	}
	post.Header.Set("Content-Type", "application/json")
	response, err := client.Do(post)
	if err != nil {
		return fmt.Errorf("reach the server at %s: %w", socket, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read the fixture answer: %w", err)
	}
	_, _ = stdout.Write(answer)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", name, response.StatusCode)
	}
	return nil
}

// fixtureRequest reads a fixture subcommand's arguments into the request the server takes.
func fixtureRequest(name string, args []string) (FixtureRequest, error) {
	byCluster := map[string]bool{
		"latest-report": true, "revoke-keys": true, "delete-cluster": true, "expire-cursors": true,
	}
	switch {
	case name == "mint-token":
		flags := flag.NewFlagSet(name, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		grn := flags.String("cluster", "", "")
		expired := flags.Bool("expired", false, "")
		if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
			return FixtureRequest{}, usageError("mint-token takes only --cluster and --expired")
		}
		return FixtureRequest{Cluster: GRN(*grn), Expired: *expired}, nil
	case len(args) != 1:
		return FixtureRequest{}, usageError(fmt.Sprintf("%s takes one argument", name))
	case name == "declare":
		read, err := os.ReadFile(args[0])
		if err != nil {
			return FixtureRequest{}, fmt.Errorf("read %s: %w", args[0], err)
		}
		var q FixtureRequest
		if err := json.Unmarshal(read, &q); err != nil {
			return FixtureRequest{}, fmt.Errorf("%s is not a declare file: %w", args[0], err)
		}
		return q, nil
	case name == "tombstone":
		return FixtureRequest{ID: args[0]}, nil
	case byCluster[name]:
		return FixtureRequest{Cluster: GRN(strings.TrimSpace(args[0]))}, nil
	}
	return FixtureRequest{}, usageError(fmt.Sprintf("unknown subcommand %q", name))
}
