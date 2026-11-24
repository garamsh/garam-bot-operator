// Package secret keeps a cluster's identity in one Kubernetes Secret in the release namespace (ADR 0088
// decision 4, ADR 0091 decision 5).
package secret

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
)

// The Secret's data keys.
const (
	keyData       = "key.pem"
	successorData = "successor.pem"
	clusterData   = "cluster"
	clientIDData  = "clientId"
	tokenURLData  = "tokenUrl"
	keySinceData  = "keySince"
	attemptedData = "registrationAttempt"
)

// Store is a clusteridentity.KeyStore over one Secret the chart creates empty. It never creates a Secret,
// so its Role needs only get and update, on that Secret's name alone.
type Store struct {
	client    kubernetes.Interface
	namespace string
	name      string

	mu sync.Mutex
	// read is the Secret as last read or written here. A Save updates it on its resourceVersion alone.
	read *corev1.Secret
}

var _ clusteridentity.KeyStore = (*Store)(nil)

// New returns a Store over the Secret name in namespace.
func New(client kubernetes.Interface, namespace, name string) *Store {
	return &Store{client: client, namespace: namespace, name: name}
}

// Load answers the State the Secret holds, or clusteridentity.ErrNoState where the Secret holds no data.
// A missing Secret is an error: the chart creates it, and the connector does not.
func (s *Store) Load(ctx context.Context) (clusteridentity.State, error) {
	found, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return clusteridentity.State{}, fmt.Errorf("read the identity Secret %s/%s: %w", s.namespace, s.name, err)
	}
	s.mu.Lock()
	s.read = found
	s.mu.Unlock()
	if len(found.Data) == 0 {
		return clusteridentity.State{}, clusteridentity.ErrNoState
	}
	return decode(found.Data)
}

// Save writes state into the Secret, on the condition that nobody wrote it after the last Load or Save
// here; otherwise it answers clusteridentity.ErrAnotherWriter.
func (s *Store) Save(ctx context.Context, state clusteridentity.State) error {
	data, err := encode(state)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.read == nil {
		return errors.New("the identity Secret is saved only after it is loaded")
	}
	updated := s.read.DeepCopy()
	updated.Data = data
	written, err := s.client.CoreV1().Secrets(s.namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("write the identity Secret %s/%s: %w", s.namespace, s.name, clusteridentity.ErrAnotherWriter)
	}
	if err != nil {
		return fmt.Errorf("write the identity Secret %s/%s: %w", s.namespace, s.name, err)
	}
	s.read = written
	return nil
}

func encode(state clusteridentity.State) (map[string][]byte, error) {
	key, err := encodeKey(state.Key)
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{keyData: key}
	if state.Successor != nil {
		if data[successorData], err = encodeKey(state.Successor); err != nil {
			return nil, err
		}
	}
	if state.Record != nil {
		data[clusterData] = []byte(state.Record.Cluster)
		data[clientIDData] = []byte(state.Record.ClientID)
		data[tokenURLData] = []byte(state.Record.TokenURL)
		data[keySinceData] = []byte(state.KeySince.UTC().Format(time.RFC3339))
	}
	if state.Attempted != "" {
		data[attemptedData] = []byte(state.Attempted)
	}
	return data, nil
}

func decode(data map[string][]byte) (clusteridentity.State, error) {
	key, err := decodeKey(data[keyData])
	if err != nil {
		return clusteridentity.State{}, fmt.Errorf("%s: %w", keyData, err)
	}
	state := clusteridentity.State{Key: key, Attempted: string(data[attemptedData])}
	if encoded, ok := data[successorData]; ok {
		if state.Successor, err = decodeKey(encoded); err != nil {
			return clusteridentity.State{}, fmt.Errorf("%s: %w", successorData, err)
		}
	}
	if _, ok := data[clusterData]; ok {
		state.Record = &clusteridentity.Record{
			Cluster:  clusteridentity.GRN(data[clusterData]),
			ClientID: string(data[clientIDData]),
			TokenURL: string(data[tokenURLData]),
		}
		if state.Record.ClientID == "" || state.Record.TokenURL == "" {
			return clusteridentity.State{}, errors.New("the identity Secret names a cluster without its clientId and tokenUrl")
		}
		if state.KeySince, err = time.Parse(time.RFC3339, string(data[keySinceData])); err != nil {
			return clusteridentity.State{}, fmt.Errorf("%s is not an RFC 3339 time", keySinceData)
		}
	}
	return state, nil
}

func encodeKey(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode a key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// decodeKey parses a PKCS#8 RSA key. Its errors name structures, never key material.
func decodeKey(encoded []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("holds no PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("is not a PKCS#8 private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("is not an RSA key")
	}
	return key, nil
}
