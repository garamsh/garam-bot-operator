package secret_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

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

// created is the Secret as the chart creates it: empty.
func created() *corev1.Secret {
	return &corev1.Secret{Name: name, Namespace: namespace, Type: corev1.SecretTypeOpaque, ResourceVersion: "1"}
}

// clientset is a fake clientset holding objects, which refuses an update as the API server does: one
// whose resourceVersion is not the stored one is a conflict, and each update stored is a new version.
// The fake's tracker checks no resourceVersion itself.
func clientset(objects ...runtime.Object) *fake.Clientset {
	client := fake.NewClientset(objects...)
	client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(clienttesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		incoming, ok := update.GetObject().(*corev1.Secret)
		if !ok {
			return false, nil, nil
		}
		stored, err := client.Tracker().Get(action.GetResource(), incoming.Namespace, incoming.Name)
		if err != nil {
			return false, nil, nil //nolint:nilerr // The tracker answers the missing object itself.
		}
		current, ok := stored.(*corev1.Secret)
		if !ok {
			return false, nil, nil
		}
		if incoming.ResourceVersion != current.ResourceVersion {
			return true, nil, apierrors.NewConflict(action.GetResource().GroupResource(), incoming.Name,
				errors.New("the object has been modified"))
		}
		version, err := strconv.Atoi(current.ResourceVersion)
		if err != nil {
			return true, nil, err
		}
		incoming.ResourceVersion = strconv.Itoa(version + 1)
		return false, nil, nil
	})
	return client
}

func TestStore_LoadAnswersErrNoStateFromTheSecretTheChartCreated(t *testing.T) {
	store := secret.New(clientset(created()), namespace, name)

	_, err := store.Load(context.Background())

	assert.ErrorIs(t, err, clusteridentity.ErrNoState)
}

func TestStore_NeverCreatesTheSecret(t *testing.T) {
	client := clientset()
	store := secret.New(client, namespace, name)

	_, err := store.Load(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, clusteridentity.ErrNoState, "a missing Secret is the chart's error, not a cluster to register")
	require.Error(t, store.Save(context.Background(), clusteridentity.State{Key: key(t)}))
	for _, action := range client.Actions() {
		assert.NotEqual(t, "create", action.GetVerb(), "the Role grants no create")
	}
}

func TestStore_KeepsEveryStateItIsGiven(t *testing.T) {
	since := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	record := &clusteridentity.Record{
		Cluster: "grn:acme/iam/cluster/a1", ClientID: "grn:acme/iam/cluster/a1", TokenURL: "https://garam.example/oauth/token",
	}
	for test, state := range map[string]clusteridentity.State{
		"a key before registration":     {Key: key(t), Attempted: "ab12"},
		"a registered record":           {Key: key(t), Record: record, KeySince: since},
		"a rotation stored, unresolved": {Key: key(t), Record: record, KeySince: since, Successor: key(t)},
	} {
		t.Run(test, func(t *testing.T) {
			client := clientset(created())
			store := secret.New(client, namespace, name)
			_, err := store.Load(context.Background())
			require.ErrorIs(t, err, clusteridentity.ErrNoState, "setup")
			require.NoError(t, store.Save(context.Background(), clusteridentity.State{Key: key(t)}),
				"setup: an earlier State, so the second Save follows the first")

			require.NoError(t, store.Save(context.Background(), state))
			got, err := secret.New(client, namespace, name).Load(context.Background())

			require.NoError(t, err)
			assert.True(t, state.Key.Equal(got.Key))
			assert.Equal(t, state.Successor != nil, got.Successor != nil)
			if state.Successor != nil {
				assert.True(t, state.Successor.Equal(got.Successor))
			}
			assert.Equal(t, state.Record, got.Record)
			assert.True(t, state.KeySince.Equal(got.KeySince))
			assert.Equal(t, state.Attempted, got.Attempted)
		})
	}
}

func TestStore_RefusesToWriteOverAnotherWriter(t *testing.T) {
	client := clientset(created())
	first, second := secret.New(client, namespace, name), secret.New(client, namespace, name)
	for _, store := range []*secret.Store{first, second} {
		_, err := store.Load(context.Background())
		require.ErrorIs(t, err, clusteridentity.ErrNoState, "setup: both read the same Secret")
	}
	written := key(t)
	require.NoError(t, first.Save(context.Background(), clusteridentity.State{Key: written}))

	err := second.Save(context.Background(), clusteridentity.State{Key: key(t)})

	require.ErrorIs(t, err, clusteridentity.ErrAnotherWriter)
	got, err := second.Load(context.Background())
	require.NoError(t, err)
	assert.True(t, written.Equal(got.Key), "what the first wrote is kept")
	require.NoError(t, second.Save(context.Background(), clusteridentity.State{Key: key(t)}),
		"control: once it has read what the other wrote, it writes")
}

func TestStore_KeepsWhatTheChartPutOnTheSecret(t *testing.T) {
	labelled := created()
	labelled.Labels = map[string]string{"app.kubernetes.io/instance": "garam"}
	client := clientset(labelled)
	store := secret.New(client, namespace, name)
	_, err := store.Load(context.Background())
	require.ErrorIs(t, err, clusteridentity.ErrNoState, "setup")

	require.NoError(t, store.Save(context.Background(), clusteridentity.State{Key: key(t)}))

	stored, err := client.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, labelled.Labels, stored.Labels)
	assert.Equal(t, corev1.SecretTypeOpaque, stored.Type)
}

func TestStore_RefusesASecretWithoutAKey(t *testing.T) {
	client := clientset(&corev1.Secret{
		Name:      name,
		Namespace: namespace,
		// A whole record, so that the missing key is the only thing wrong.
		Data: map[string][]byte{
			"cluster": []byte("grn:acme/iam/cluster/a1"), "clientId": []byte("grn:acme/iam/cluster/a1"),
			"tokenUrl": []byte("https://garam.example/oauth/token"), "keySince": []byte("2026-11-25T09:00:00Z"),
		},
	})
	store := secret.New(client, namespace, name)

	_, err := store.Load(context.Background())

	require.Error(t, err)
	assert.NotErrorIs(t, err, clusteridentity.ErrNoState, "a damaged Secret is not an empty one, so nothing registers over it")
}
