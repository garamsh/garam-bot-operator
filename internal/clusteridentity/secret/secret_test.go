package secret_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity/secret"
)

// namespace and name are where every test keeps its Secret.
const (
	namespace = "garam"
	name      = "identity"
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return k
}

func TestStore_LoadAnswersErrNoStateWithoutASecret(t *testing.T) {
	store := secret.Store{Client: fake.NewClientset(), Namespace: namespace, Name: name}

	_, err := store.Load(context.Background())

	assert.ErrorIs(t, err, clusteridentity.ErrNoState)
}

func TestStore_KeepsEveryStateItIsGiven(t *testing.T) {
	since := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	record := &clusteridentity.Record{
		Cluster: "grn:acme/iam/cluster/a1", ClientID: "grn:acme/iam/cluster/a1", TokenURL: "https://garam.example/oauth/token",
	}
	for name, state := range map[string]clusteridentity.State{
		"a key before registration":     {Key: key(t), Attempted: "ab12"},
		"a registered record":           {Key: key(t), Record: record, KeySince: since},
		"a rotation stored, unresolved": {Key: key(t), Record: record, KeySince: since, Successor: key(t)},
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientset()
			store := secret.Store{Client: client, Namespace: namespace, Name: name}
			require.NoError(t, store.Save(context.Background(), clusteridentity.State{Key: key(t)}),
				"setup: an earlier State, so the second Save updates")

			require.NoError(t, store.Save(context.Background(), state))
			got, err := store.Load(context.Background())

			require.NoError(t, err)
			assert.True(t, state.Key.Equal(got.Key))
			assert.Equal(t, state.Successor != nil, got.Successor != nil)
			if state.Successor != nil {
				assert.True(t, state.Successor.Equal(got.Successor))
			}
			assert.Equal(t, state.Record, got.Record)
			assert.True(t, state.KeySince.Equal(got.KeySince))
			assert.Equal(t, state.Attempted, got.Attempted)
			stored, err := client.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, corev1.SecretTypeOpaque, stored.Type)
		})
	}
}

func TestStore_RefusesASecretWithoutAKey(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		Name:      name,
		Namespace: namespace,
		// A whole record, so that the missing key is the only thing wrong.
		Data: map[string][]byte{
			"cluster": []byte("grn:acme/iam/cluster/a1"), "clientId": []byte("grn:acme/iam/cluster/a1"),
			"tokenUrl": []byte("https://garam.example/oauth/token"), "keySince": []byte("2026-11-25T09:00:00Z"),
		},
	})
	store := secret.Store{Client: client, Namespace: namespace, Name: name}

	_, err := store.Load(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, clusteridentity.ErrNoState, "a damaged Secret is not an absent one, so nothing registers over it")
}
