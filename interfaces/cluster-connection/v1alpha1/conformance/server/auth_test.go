//go:build conformance

package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/suite"
	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

func refusesMissingToken(t *testing.T) {
	record, bearer := target.Connect(t)
	calls := map[string]func(bearer string) wire.Answer{
		"declarations": func(bearer string) wire.Answer { return target.Poll(t, bearer, "", 0) },
		"reports": func(bearer string) wire.Answer {
			return target.Post(t, wire.ReportsPath, bearer, emptyReport())
		},
		"keys": func(bearer string) wire.Answer {
			return target.Post(t, wire.KeysPath, bearer, rotationBody(t, record, record.Key, wire.Key(t)))
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			missing := call("")

			assert.Equal(t, http.StatusUnauthorized, missing.Status, string(missing.Body))
			assert.Equal(t, "unauthorized", missing.ErrorCode(t))
			assert.Equal(t, http.StatusUnauthorized, call("not-a-token").Status, "a token the server never issued")
		})
	}
	assert.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "control: the record's own token is taken")
}

func refusesAfterClusterDeleted(t *testing.T) {
	record, bearer := target.Connect(t)
	require.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "control: live before the deletion")

	target.Fix(t, "delete-cluster", record.Cluster)

	assert.Equal(t, http.StatusUnauthorized, target.Poll(t, bearer, "", 0).Status)
	assert.Equal(t, http.StatusUnauthorized, target.Post(t, wire.ReportsPath, bearer, emptyReport()).Status)
}

func refusesAfterKeyRevoked(t *testing.T) {
	record, bearer := target.Connect(t)
	require.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "control: live before the revocation")

	target.Fix(t, "revoke-keys", record.Cluster)

	assert.Equal(t, http.StatusUnauthorized, target.Poll(t, bearer, "", 0).Status, "AUTH-2: read on every request")
	assert.NotEqual(t, http.StatusOK, target.TryExchange(t, record, record.Key).Status, "no new token either")
}

func tokenReachesNoOtherOperation(t *testing.T) {
	suite.Pending(t, "AUTH-4 is about garam's other machine operations, which neither this protocol nor "+
		"its fixture names, so this suite cannot reach one")
}

func mintOnlyBotsPlacedOnRecord(t *testing.T) {
	suite.Pending(t, "AUTH-5's bot-token mint is garam's, and its shape is not part of this interface")
}
