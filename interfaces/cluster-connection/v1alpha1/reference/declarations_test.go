package reference_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// connected registers a record and answers it with a live token.
func (h *harness) connected() (reference.GRN, string) {
	h.t.Helper()
	grn := h.register(testKeys()[0])
	return grn, h.exchange(grn, testKeys()[0])
}

func TestDeclarations_FullSetWithoutCursor(t *testing.T) {
	h := newHarness(t, pageSize(2))
	grn, bearer := h.connected()
	a, b, c := h.declare(grn, "", bot("a")), h.declare(grn, "", bot("b")), h.declare(grn, "", bot("c"))
	gone := h.declare(grn, "", bot("gone"))
	h.fix("tombstone", reference.FixtureRequest{ID: gone})

	first := h.poll(bearer, "", 0).page(t)
	second := h.poll(bearer, first.Cursor, 0).page(t)

	assert.True(t, first.Full)
	assert.True(t, first.More)
	assert.Equal(t, []string{"apply:" + a, "apply:" + b}, first.ids())
	assert.True(t, second.Full)
	assert.False(t, second.More)
	assert.Equal(t, []string{"apply:" + c}, second.ids(), "the full set names live declarations only")
}

func TestDeclarations_FullSetOfAnotherRecordIsNotSent(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	_, otherRecord := h.mint()
	own := h.declare(grn, "", bot("own"))
	h.declare(otherRecord, "", bot("foreign"))

	assert.Equal(t, []string{"apply:" + own}, h.poll(bearer, "", 0).page(t).ids())
}

func TestDeclarations_TombstoneBetweenFullPagesFollowsTheFullSet(t *testing.T) {
	h := newHarness(t, pageSize(1))
	grn, bearer := h.connected()
	a, b := h.declare(grn, "", bot("a")), h.declare(grn, "", bot("b"))

	first := h.poll(bearer, "", 0).page(t)
	require.Equal(t, []string{"apply:" + a}, first.ids())
	h.fix("tombstone", reference.FixtureRequest{ID: a})
	h.declare(grn, b, bot("b"))
	second := h.poll(bearer, first.Cursor, 0).page(t)
	after := h.poll(bearer, second.Cursor, 0).page(t)

	assert.Equal(t, []string{"apply:" + b}, second.ids())
	assert.EqualValues(t, 2, second.Changes[0].Generation, "the full set sends b as last written")
	assert.False(t, second.More)
	assert.Contains(t, after.ids(), "delete:"+a,
		"the tombstone written while the full set was read, before b's later write")
}

func TestDeclarations_ChangesInWriteOrder(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	a := h.declare(grn, "", bot("a"))
	cursor := h.poll(bearer, "", 0).page(t).Cursor

	b := h.declare(grn, "", bot("b"))
	h.declare(grn, a, bot("a"))
	h.fix("tombstone", reference.FixtureRequest{ID: b})
	got := h.poll(bearer, cursor, 0).page(t)

	assert.False(t, got.Full)
	assert.Equal(t, []string{"apply:" + b, "apply:" + a, "delete:" + b}, got.ids())
	assert.EqualValues(t, 2, got.Changes[1].Generation)
}

func TestDeclarations_PagesChangesWithMore(t *testing.T) {
	h := newHarness(t, pageSize(1))
	grn, bearer := h.connected()
	cursor := h.poll(bearer, "", 0).page(t).Cursor
	a, b := h.declare(grn, "", bot("a")), h.declare(grn, "", bot("b"))

	first := h.poll(bearer, cursor, 0).page(t)
	second := h.poll(bearer, first.Cursor, 0).page(t)

	assert.True(t, first.More)
	assert.Equal(t, []string{"apply:" + a}, first.ids())
	assert.False(t, second.More)
	assert.Equal(t, []string{"apply:" + b}, second.ids())
}

func TestDeclarations_AnswersAtOnceWhenPending(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	cursor := h.poll(bearer, "", 0).page(t).Cursor
	h.declare(grn, "", bot("a"))

	started := time.Now()
	got := h.poll(bearer, cursor, 50).page(t)

	assert.Len(t, got.Changes, 1)
	assert.Less(t, time.Since(started), time.Second)
}

func TestDeclarations_WakesHeldPollOnWrite(t *testing.T) {
	for name, write := range map[string]func(h *harness, grn reference.GRN, id string){
		"declare":   func(h *harness, grn reference.GRN, _ string) { h.declare(grn, "", bot("b")) },
		"tombstone": func(h *harness, _ reference.GRN, id string) { h.fix("tombstone", reference.FixtureRequest{ID: id}) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			grn, bearer := h.connected()
			a := h.declare(grn, "", bot("a"))
			cursor := h.poll(bearer, "", 0).page(t).Cursor
			done := make(chan answer, 1)
			go func() { done <- h.poll(bearer, cursor, 30) }()
			time.Sleep(200 * time.Millisecond)
			select {
			case <-done:
				t.Fatal("setup: the poll was answered before any write")
			default:
			}

			written := time.Now()
			write(h, grn, a)

			select {
			case got := <-done:
				assert.Len(t, got.page(t).Changes, 1)
				assert.Less(t, time.Since(written), time.Second, "DECL-3: within a second of the write")
			case <-time.After(2 * time.Second):
				t.Fatal("the held poll was not answered within 2 seconds of the write")
			}
		})
	}
}

func TestDeclarations_EmptyAnswerKeepsCursor(t *testing.T) {
	h := newHarness(t)
	_, bearer := h.connected()
	cursor := h.poll(bearer, "", 0).page(t).Cursor

	started := time.Now()
	got := h.poll(bearer, cursor, 1)

	assert.Equal(t, cursor, got.page(t).Cursor)
	assert.Equal(t, []any{}, got.json(t)["changes"], "changes is an empty array, not absent")
	assert.GreaterOrEqual(t, time.Since(started), time.Second, "the poll was held for wait")
}

func TestDeclarations_TombstoneCarriesID(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	a := h.declare(grn, "", bot("a"))
	cursor := h.poll(bearer, "", 0).page(t).Cursor

	h.fix("tombstone", reference.FixtureRequest{ID: a})
	got := h.poll(bearer, cursor, 0).page(t)

	require.Len(t, got.Changes, 1)
	assert.Equal(t, "delete", got.Changes[0].Op)
	assert.Equal(t, a, got.Changes[0].ID)
	assert.EqualValues(t, 2, got.Changes[0].Generation, "past the generation last applied")
	assert.Empty(t, got.Changes[0].Object)
}

func TestDeclarations_ExpiredCursorIs410(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	a := h.declare(grn, "", bot("a"))
	cursor := h.poll(bearer, "", 0).page(t).Cursor
	h.fix("tombstone", reference.FixtureRequest{ID: a})
	require.Equal(t, http.StatusOK, h.poll(bearer, cursor, 0).status, "control: live before the purge")

	h.fix("expire-cursors", reference.FixtureRequest{Cluster: grn})
	got := h.poll(bearer, cursor, 0)

	assert.Equal(t, http.StatusGone, got.status)
	assert.Equal(t, "cursor_expired", got.errorCode(t))
	reread := h.poll(bearer, "", 0).page(t)
	assert.Equal(t, http.StatusOK, h.poll(bearer, reread.Cursor, 0).status,
		"the full set read again answers a live cursor")
}

func TestDeclarations_ExpiredCursorIs410WithNoTombstoneLeft(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	h.declare(grn, "", bot("a"))
	cursor := h.poll(bearer, "", 0).page(t).Cursor

	h.fix("expire-cursors", reference.FixtureRequest{Cluster: grn})

	assert.Equal(t, http.StatusGone, h.poll(bearer, cursor, 0).status)
}

func TestDeclarations_RefusesMalformedQuery(t *testing.T) {
	h := newHarness(t)
	_, bearer := h.connected()
	for name, path := range map[string]string{
		"wait over 50":          declPath + "?wait=51",
		"wait negative":         declPath + "?wait=-1",
		"cursor of no position": declPath + "?wait=0&after=c.99",
		"cursor not a cursor":   declPath + "?wait=0&after=nonsense",
	} {
		t.Run(name, func(t *testing.T) {
			got := h.do(http.MethodGet, path, bearer, "", nil)
			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Equal(t, "invalid_request", got.errorCode(t))
		})
	}
}

func TestDeclare_RefusesObjectsBreakingDECL7To9(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	with := func(change func(map[string]any)) map[string]any {
		object := bot("x")
		change(object)
		return object
	}
	spec := func(o map[string]any) map[string]any { return o["spec"].(map[string]any) }
	cases := map[string]map[string]any{
		"DECL-7 undeclared kind":  with(func(o map[string]any) { o["kind"] = "Secret" }),
		"DECL-7 no spec":          with(func(o map[string]any) { delete(o, "spec") }),
		"DECL-7 no namespace":     with(func(o map[string]any) { o["metadata"] = map[string]any{"name": "x"} }),
		"DECL-8 secret value":     with(func(o map[string]any) { spec(o)["tokenSecretRef"] = map[string]any{"value": "x"} }),
		"DECL-8 reference no key": with(func(o map[string]any) { spec(o)["tokenSecretRef"] = "bot-tokens" }),
		"DECL-8 key of dots":      with(func(o map[string]any) { spec(o)["tokenSecretRef"] = "bot-tokens/.." }),
		"DECL-8 nested secret":    with(func(o map[string]any) { spec(o)["env"] = []any{map[string]any{"apiSecret": "x"}} }),
		"DECL-9 image":            with(func(o map[string]any) { spec(o)["image"] = "registry.example/bot:1" }),
		"DECL-9 nested image":     with(func(o map[string]any) { spec(o)["runtime"] = map[string]any{"sidecarImage": "x"} }),
	}
	for name, object := range cases {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(object)
			require.NoError(t, err)
			status, body := h.tryFix("declare", reference.FixtureRequest{Cluster: grn, Object: encoded})
			assert.Equal(t, http.StatusBadRequest, status, string(body))
		})
	}
	valid := h.declare(grn, "", bot("valid"))
	assert.Equal(t, []string{"apply:" + valid}, h.poll(bearer, "", 0).page(t).ids(),
		"control: a valid object is stored and sent, and none of the refused ones is")
}

func TestDeclare_SendsOnlyDECL7Members(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	object := bot("a")
	object["status"] = map[string]any{"phase": "Running"}
	h.declare(grn, "", object)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(h.poll(bearer, "", 0).page(t).Changes[0].Object, &sent))

	assert.NotContains(t, sent, "status")
	assert.Equal(t, "Bot", sent["kind"])
	assert.Equal(t, "bot-tokens/token", sent["spec"].(map[string]any)["tokenSecretRef"])
}
