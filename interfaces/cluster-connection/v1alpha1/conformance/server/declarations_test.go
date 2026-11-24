//go:build conformance

package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

// bot is a Bot interface 1's schema admits, naming its one secret by reference (DECL-8).
func bot(name string) map[string]any {
	return map[string]any{
		"apiVersion": "bot.garam.sh/v1alpha1", "kind": "Bot",
		"metadata": map[string]any{"namespace": "bots", "name": name},
		"spec": map[string]any{
			"persona": map[string]any{"role": "reviewer"}, "agent": map[string]any{"harness": "claude-code"},
			"permissions": map[string]any{"secrets": []any{"bot-tokens/token"}},
		},
	}
}

func fullSetWithoutCursor(t *testing.T) {
	record, bearer := target.Connect(t)
	other, _ := target.Connect(t)
	want := make([]string, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		want = append(want, "apply:"+target.MustDeclare(t, record.Cluster, "", bot(name)))
	}
	target.MustDeclare(t, other.Cluster, "", bot("theirs"))

	pages := target.FullSet(t, bearer)

	var got []string
	for _, page := range pages {
		assert.True(t, page.Full, "DECL-1: each page of the full set says full")
		got = append(got, page.Ops()...)
	}
	assert.ElementsMatch(t, want, got, "DECL-1: every live declaration of the record, and no other record's")
}

func changesInWriteOrder(t *testing.T) {
	record, bearer := target.Connect(t)
	a := target.MustDeclare(t, record.Cluster, "", bot("a"))
	cursor := lastCursor(target.FullSet(t, bearer))

	b := target.MustDeclare(t, record.Cluster, "", bot("b"))
	target.MustDeclare(t, record.Cluster, a, bot("a"))
	target.Fix(t, "tombstone", b)
	got := target.Poll(t, bearer, cursor, 0).Page(t)

	assert.False(t, got.Full)
	assert.Equal(t, []string{"apply:" + b, "apply:" + a, "delete:" + b}, got.Ops())
	if assert.Len(t, got.Changes, 3) {
		assert.EqualValues(t, 2, got.Changes[1].Generation, "a's second write is its next generation")
	}
}

func answersAtOnceWhenPending(t *testing.T) {
	record, bearer := target.Connect(t)
	cursor := lastCursor(target.FullSet(t, bearer))
	id := target.MustDeclare(t, record.Cluster, "", bot("a"))
	start := time.Now()

	got := target.Poll(t, bearer, cursor, 50).Page(t)

	assert.Less(t, time.Since(start), 5*time.Second, "DECL-3: a pending change is answered at once")
	assert.Equal(t, []string{"apply:" + id}, got.Ops())
}

func wakesHeldPollOnWrite(t *testing.T) {
	record, bearer := target.Connect(t)
	cursor := lastCursor(target.FullSet(t, bearer))
	answered := make(chan wire.Answer, 1)
	go func() { answered <- target.Poll(t, bearer, cursor, 50) }()
	time.Sleep(time.Second)
	select {
	case got := <-answered:
		require.FailNow(t, "setup: the poll was answered before any change was written", string(got.Body))
	default:
	}

	id := target.MustDeclare(t, record.Cluster, "", bot("a"))
	written := time.Now()

	select {
	case got := <-answered:
		assert.Less(t, time.Since(written), 5*time.Second, "DECL-3: answered as soon as a change is written")
		assert.Equal(t, []string{"apply:" + id}, got.Page(t).Ops())
	case <-time.After(55 * time.Second):
		t.Fatal("DECL-3: the held poll was not answered when the change was written")
	}
}

func emptyAnswerKeepsCursor(t *testing.T) {
	record, bearer := target.Connect(t)
	target.MustDeclare(t, record.Cluster, "", bot("a"))
	cursor := lastCursor(target.FullSet(t, bearer))

	got := target.Poll(t, bearer, cursor, 0).Page(t)

	assert.Equal(t, cursor, got.Cursor, "DECL-4")
	assert.NotNil(t, got.Changes, "DECL-4: changes is present and empty")
	assert.Empty(t, got.Changes)
}

func tombstoneCarriesID(t *testing.T) {
	record, bearer := target.Connect(t)
	id := target.MustDeclare(t, record.Cluster, "", bot("a"))
	cursor := lastCursor(target.FullSet(t, bearer))

	target.Fix(t, "tombstone", id)
	got := target.Poll(t, bearer, cursor, 0).Page(t)

	assert.Equal(t, []string{"delete:" + id}, got.Ops())
}

func expiredCursorIs410(t *testing.T) {
	record, bearer := target.Connect(t)
	id := target.MustDeclare(t, record.Cluster, "", bot("a"))
	cursor := lastCursor(target.FullSet(t, bearer))
	target.Fix(t, "tombstone", id)
	require.Equal(t, http.StatusOK, target.Poll(t, bearer, cursor, 0).Status, "control: live before the purge")

	target.Fix(t, "expire-cursors", record.Cluster)
	got := target.Poll(t, bearer, cursor, 0)

	assert.Equal(t, http.StatusGone, got.Status, string(got.Body))
	assert.Equal(t, "cursor_expired", got.ErrorCode(t))
	assert.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "DECL-6: the full set is read instead")
}

func expiredCursorIs410WithNoTombstoneLeft(t *testing.T) {
	record, bearer := target.Connect(t)
	target.MustDeclare(t, record.Cluster, "", bot("a"))
	cursor := lastCursor(target.FullSet(t, bearer))

	target.Fix(t, "expire-cursors", record.Cluster)
	got := target.Poll(t, bearer, cursor, 0)

	assert.Equal(t, http.StatusGone, got.Status, "DECL-6: whether or not any tombstone remains: %s", got.Body)
	assert.Equal(t, "cursor_expired", got.ErrorCode(t))
}

// refusedDeclarations requires the fixture to refuse each object, none of which is then sent, and, as a
// control, a valid one to be stored and sent.
func refusedDeclarations(t *testing.T, cases map[string]map[string]any) {
	t.Helper()
	record, bearer := target.Connect(t)
	for name, object := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := target.Declare(t, record.Cluster, "", object)
			assert.Error(t, err, "the fixture refuses it")
		})
	}
	valid := target.MustDeclare(t, record.Cluster, "", bot("valid"))
	var sent []string
	for _, page := range target.FullSet(t, bearer) {
		sent = append(sent, page.Ops()...)
	}
	assert.Equal(t, []string{"apply:" + valid}, sent, "control: the valid object is stored and sent, and no refused one")
}

func withBot(change func(object, spec map[string]any)) map[string]any {
	object := bot("x")
	change(object, object["spec"].(map[string]any))
	return object
}

func refusesUndeclaredKind(t *testing.T) {
	refusedDeclarations(t, map[string]map[string]any{
		"Secret": withBot(func(o, _ map[string]any) {
			o["apiVersion"], o["kind"] = "v1", "Secret"
		}),
		"ConfigMap": withBot(func(o, _ map[string]any) {
			o["apiVersion"], o["kind"] = "v1", "ConfigMap"
		}),
		"Deployment": withBot(func(o, _ map[string]any) {
			o["apiVersion"], o["kind"] = "apps/v1", "Deployment"
		}),
		"a declared kind in another group": withBot(func(o, _ map[string]any) {
			o["apiVersion"] = "example.com/v1alpha1"
		}),
	})
}

func refusesSecretValue(t *testing.T) {
	secrets := func(refs ...any) map[string]any {
		return withBot(func(_, spec map[string]any) { spec["permissions"] = map[string]any{"secrets": refs} })
	}
	refusedDeclarations(t, map[string]map[string]any{
		"a bare secret value":            secrets("sk-live-0123456789abcdef"),
		"a value under a known name":     secrets(map[string]any{"value": "sk-live-0123456789abcdef"}),
		"a reference with no key":        secrets("bot-tokens"),
		"a key of dots":                  secrets("bot-tokens/.."),
		"a reference naming a namespace": secrets("other/bot-tokens/token"),
	})
}

func refusesImage(t *testing.T) {
	refusedDeclarations(t, map[string]map[string]any{
		"on the agent": withBot(func(_, spec map[string]any) {
			spec["agent"].(map[string]any)["image"] = "registry.example/bot:1"
		}),
		"on the spec": withBot(func(_, spec map[string]any) { spec["image"] = "registry.example/bot:1" }),
	})
}

func lastCursor(pages []wire.Page) string {
	return pages[len(pages)-1].Cursor
}
