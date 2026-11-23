package reference_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTransport_IgnoresUnknownMembers(t *testing.T) {
	h := newHarness(t)
	token, _ := h.mint()
	body := registration(token, publicJWK(t, testKeys()[0]))
	body["futureMember"] = map[string]any{"nested": true}
	body["connector"].(map[string]any)["futureMember"] = 1

	got := h.post(regPath, "", body)

	assert.Equal(t, http.StatusCreated, got.status, string(got.body))
	grn, bearer := h.connected()
	rep := report(h.clock.Now(), nil, nil)
	rep["futureMember"] = "x"
	assert.Equal(t, http.StatusNoContent, h.post(repPath, bearer, rep).status)
	assert.NotNil(t, h.latest(grn))
}

func TestTransport_ErrorBodyShape(t *testing.T) {
	h := newHarness(t)
	for name, got := range map[string]answer{
		"401": h.poll("", "", 0),
		"400": h.do(http.MethodPost, regPath, "", "application/json", []byte("not json")),
		"404": h.do(http.MethodGet, prefix+"/nowhere", "", "", nil),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, "application/json", got.header.Get("Content-Type"))
			body := got.json(t)
			assert.IsType(t, "", body["error"])
			assert.NotEmpty(t, body["error"])
			assert.IsType(t, "", body["message"])
		})
	}
}
