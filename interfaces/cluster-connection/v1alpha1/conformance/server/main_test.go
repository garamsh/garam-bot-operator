//go:build conformance

// Package server is conformance.md's server suite. It runs against the server CONFORMANCE_BASE_URL
// names, trusting only the root CONFORMANCE_TRUST_FILE holds, and drives it through the fixture
// command CONFORMANCE_FIXTURE names.
package server

import (
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/suite"
	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

const prefix = "server/"

var (
	target    *wire.Target
	targetErr error
)

func TestMain(m *testing.M) {
	target, targetErr = wire.FromEnv()
	os.Exit(suite.Main(m))
}

// tests are the suite, in the order they run. throttle-spends-nothing is last: it spends the source's
// registration bound, which every other test registers under.
var tests = []suite.Test{
	{Name: "server/registration/accepts-fresh-token", Run: acceptsFreshToken},
	{Name: "server/registration/refuses-spent-token", Run: refusesSpentToken},
	{Name: "server/registration/refuses-unknown-token-same-as-spent", Run: refusesUnknownTokenSameAsSpent},
	{Name: "server/registration/refuses-expired-token", Run: refusesExpiredToken},
	{Name: "server/registration/spends-token-on-refused-key", Run: spendsTokenOnRefusedKey},
	{Name: "server/registration/refuses-private-jwk-members", Run: refusesPrivateJWKMembers},
	{Name: "server/registration/withdraws-earlier-keys", Run: withdrawsEarlierKeys},
	{Name: "server/registration/distinct-tokens-are-unpredictable", Run: distinctTokensAreUnpredictable},
	{Name: "server/auth/refuses-missing-token", Run: refusesMissingToken},
	{Name: "server/auth/refuses-after-cluster-deleted", Run: refusesAfterClusterDeleted},
	{Name: "server/auth/refuses-after-key-revoked", Run: refusesAfterKeyRevoked},
	{Name: "server/auth/token-reaches-no-other-operation", Run: tokenReachesNoOtherOperation},
	{Name: "server/auth/mint-only-bots-placed-on-record", Run: mintOnlyBotsPlacedOnRecord},
	{Name: "server/rotation/accepts-proved-successor", Run: acceptsProvedSuccessor},
	{Name: "server/rotation/refuses-bad-proof", Run: refusesBadProof},
	{Name: "server/rotation/refuses-replayed-proof", Run: refusesReplayedProof},
	{Name: "server/rotation/refuses-rotation-without-current-key-signature", Run: refusesUnsignedRotation},
	{Name: "server/rotation/refuses-authorization-for-another-successor", Run: refusesAuthorizationForAnotherSuccessor},
	{Name: "server/rotation/refuses-authorization-by-retired-key", Run: refusesAuthorizationByRetiredKey},
	{Name: "server/rotation/repeat-answers-first-success", Run: repeatAnswersFirstSuccess},
	{Name: "server/rotation/refuses-third-key", Run: refusesThirdKey},
	{Name: "server/rotation/refuses-key-held-before", Run: refusesKeyHeldBefore},
	{Name: "server/rotation/predecessor-live-24h", Run: predecessorLive24h},
	{Name: "server/declarations/full-set-without-cursor", Run: fullSetWithoutCursor},
	{Name: "server/declarations/changes-in-write-order", Run: changesInWriteOrder},
	{Name: "server/declarations/answers-at-once-when-pending", Run: answersAtOnceWhenPending},
	{Name: "server/declarations/wakes-held-poll-on-write", Run: wakesHeldPollOnWrite},
	{Name: "server/declarations/empty-answer-keeps-cursor", Run: emptyAnswerKeepsCursor},
	{Name: "server/declarations/tombstone-carries-id", Run: tombstoneCarriesID},
	{Name: "server/declarations/expired-cursor-is-410", Run: expiredCursorIs410},
	{Name: "server/declarations/expired-cursor-is-410-with-no-tombstone-left", Run: expiredCursorIs410WithNoTombstoneLeft},
	{Name: "server/declarations/refuses-undeclared-kind", Run: refusesUndeclaredKind},
	{Name: "server/declarations/refuses-secret-value", Run: refusesSecretValue},
	{Name: "server/declarations/refuses-image", Run: refusesImage},
	{Name: "server/reports/accepts-report", Run: acceptsReport},
	{Name: "server/reports/keeps-latest-observed", Run: keepsLatestObserved},
	{Name: "server/reports/records-last-contact", Run: recordsLastContact},
	{Name: "server/reports/refuses-secret-kind", Run: refusesSecretKind},
	{Name: "server/reports/refuses-undeclared-kind", Run: refusesUndeclaredReportKind},
	{Name: "server/reports/ignores-foreign-declaration-id", Run: ignoresForeignDeclarationID},
	{Name: "server/reports/spec-is-never-a-declaration", Run: specIsNeverADeclaration},
	{Name: "server/transport/ignores-unknown-members", Run: ignoresUnknownMembers},
	{Name: "server/transport/error-body-shape", Run: errorBodyShape},
	{Name: "server/registration/throttle-spends-nothing", Run: throttleSpendsNothing},
}

// TestServer runs the suite under conformance.md's names.
func TestServer(t *testing.T) {
	require.NoError(t, targetErr, "the server under test")
	suite.Run(t, prefix, tests)
}

// TestServer_NamesEveryTestOfConformanceMd holds the suite to conformance.md: each test it names exists,
// under its name, and no other.
func TestServer_NamesEveryTestOfConformanceMd(t *testing.T) {
	named, err := suite.Names("../../conformance.md", prefix)
	require.NoError(t, err)
	require.NotEmpty(t, named, "control: conformance.md is read")
	built := make([]string, 0, len(tests))
	for _, test := range tests {
		built = append(built, test.Name)
	}
	slices.Sort(named)
	slices.Sort(built)
	assert.Equal(t, named, built)
}
