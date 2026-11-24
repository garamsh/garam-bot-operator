//go:build conformance

package server

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

func ignoresUnknownMembers(t *testing.T) {
	token, _ := target.Mint(t)
	registration := wire.Registration(t, token, wire.Key(t))
	registration["futureMember"] = map[string]any{"nested": true}
	registration["connector"].(map[string]any)["futureMember"] = 1

	got := target.Register(t, registration)

	assert.Equal(t, http.StatusCreated, got.Status, "T-4: a registration with an unknown member: %s", got.Body)
	record, bearer := target.Connect(t)
	rep := emptyReport()
	rep["futureMember"] = "x"
	rep["connector"].(map[string]any)["futureMember"] = 1
	assert.Equal(t, http.StatusNoContent, target.Post(t, reportsPath, bearer, rep).Status,
		"T-4: a report with an unknown member")
	assert.NotNil(t, latest(t, record.Cluster))
}

func errorBodyShape(t *testing.T) {
	for name, got := range map[string]wire.Answer{
		"401": target.Poll(t, "", "", 0),
		"400": target.Do(t, http.MethodPost, wire.RegistrationsPath, "", "application/json", []byte("not json")),
		"404": target.Do(t, http.MethodGet, wire.Prefix+"/nowhere", "", "", nil),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, name, strconv.Itoa(got.Status), string(got.Body))
			assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
			body := got.JSON(t)
			assert.IsType(t, "", body["error"], "T-5: error is a string")
			assert.NotEmpty(t, body["error"])
			assert.IsType(t, "", body["message"], "T-5: message is a string")
		})
	}
}
