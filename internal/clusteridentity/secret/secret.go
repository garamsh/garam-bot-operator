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

// Store is a clusteridentity.KeyStore over the Secret Name in Namespace.
type Store struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

var _ clusteridentity.KeyStore = Store{}

// Load answers the State the Secret holds, or clusteridentity.ErrNoState where there is no Secret.
func (s Store) Load(ctx context.Context) (clusteridentity.State, error) {
	found, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return clusteridentity.State{}, clusteridentity.ErrNoState
	}
	if err != nil {
		return clusteridentity.State{}, fmt.Errorf("read the identity Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return decode(found.Data)
}

// Save writes state into the Secret, creating it where there is none.
func (s Store) Save(ctx context.Context, state clusteridentity.State) error {
	data, err := encode(state)
	if err != nil {
		return err
	}
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	found, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &corev1.Secret{
			Name:      s.Name,
			Namespace: s.Namespace,
			Type:      corev1.SecretTypeOpaque,
			Data:      data,
		}, metav1.CreateOptions{})
	case err == nil:
		found.Data = data
		_, err = secrets.Update(ctx, found, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("write the identity Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
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
