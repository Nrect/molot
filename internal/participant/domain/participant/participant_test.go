// Black-box domain tests: package participant_test, table-driven, zero
// mocks; fixtures only through the domain API (BOOK_AUDIT rule 40).
package participant_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/participant/domain/participant"
)

const validID = "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f"

func mustID(t *testing.T, raw string) participant.ParticipantID {
	t.Helper()
	id, err := participant.NewParticipantID(raw)
	require.NoError(t, err)
	return id
}

func mustEmail(t *testing.T, raw string) participant.EmailAddress {
	t.Helper()
	email, err := participant.NewEmailAddress(raw)
	require.NoError(t, err)
	return email
}

func registeredParticipant(t *testing.T, email string) *participant.Participant {
	t.Helper()
	p, err := participant.Register(mustID(t, validID), mustEmail(t, email), "Boris the Bidder")
	require.NoError(t, err)
	return p
}

func TestRegister(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		t.Parallel()

		id := mustID(t, validID)
		email := mustEmail(t, "boris@example.com")

		p, err := participant.Register(id, email, "  Boris the Bidder ")
		require.NoError(t, err)

		assert.Equal(t, id, p.ID())
		assert.Equal(t, email, p.Email())
		assert.Equal(t, "Boris the Bidder", p.DisplayName(), "display name is trimmed")
		assert.Equal(t, participant.StatusRegistered, p.Status())
		assert.False(t, p.IsVerified())
		assert.Equal(t, int64(1), p.Version())

		events := p.PullDomainEvents()
		require.Len(t, events, 1)
		registered, ok := events[0].(participant.ParticipantRegistered)
		require.True(t, ok, "expected ParticipantRegistered, got %T", events[0])
		assert.Equal(t, id, registered.ID)
		assert.Equal(t, email, registered.Email)
		assert.Equal(t, "Boris the Bidder", registered.DisplayName)

		assert.Empty(t, p.PullDomainEvents(), "events are drained exactly once")
	})

	t.Run("validation", func(t *testing.T) {
		t.Parallel()

		validEmail := mustEmail(t, "boris@example.com")
		testCases := []struct {
			name        string
			id          participant.ParticipantID
			email       participant.EmailAddress
			displayName string
			wantErr     error
		}{
			{"zero id", participant.ParticipantID{}, validEmail, "Boris", participant.ErrInvalidParticipantID},
			{"zero email", mustID(t, validID), participant.EmailAddress{}, "Boris", participant.ErrInvalidEmail},
			{"empty display name", mustID(t, validID), validEmail, "", participant.ErrEmptyDisplayName},
			{"whitespace display name", mustID(t, validID), validEmail, "   ", participant.ErrEmptyDisplayName},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				p, err := participant.Register(tc.id, tc.email, tc.displayName)
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, p)
			})
		}
	})
}

func TestVerify(t *testing.T) {
	t.Parallel()

	t.Run("registered participant becomes verified once", func(t *testing.T) {
		t.Parallel()

		p := registeredParticipant(t, "boris@example.com")
		p.PullDomainEvents() // drop the registration event

		now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
		require.NoError(t, p.Verify(now))

		assert.True(t, p.IsVerified())
		assert.Equal(t, participant.StatusVerified, p.Status())

		events := p.PullDomainEvents()
		require.Len(t, events, 1)
		verified, ok := events[0].(participant.ParticipantVerified)
		require.True(t, ok, "expected ParticipantVerified, got %T", events[0])
		assert.Equal(t, p.ID(), verified.ID)
		assert.Equal(t, now.UTC(), verified.OccurredAt, "business time is normalized to UTC")
	})

	t.Run("second verification is a conflict and records nothing", func(t *testing.T) {
		t.Parallel()

		p := registeredParticipant(t, "boris@example.com")
		require.NoError(t, p.Verify(time.Now()))
		p.PullDomainEvents()

		err := p.Verify(time.Now())

		require.ErrorIs(t, err, participant.ErrAlreadyVerified)
		assert.True(t, p.IsVerified())
		assert.Empty(t, p.PullDomainEvents(), "failed transition must not record events")
	})
}

func TestUnmarshalFromDatabase(t *testing.T) {
	t.Parallel()

	t.Run("happy path carries no pending events", func(t *testing.T) {
		t.Parallel()

		p, err := participant.UnmarshalFromDatabase(validID, "boris@example.com", "Boris", "verified", 7)
		require.NoError(t, err)

		assert.Equal(t, validID, p.ID().String())
		assert.Equal(t, "boris@example.com", p.Email().String())
		assert.Equal(t, "Boris", p.DisplayName())
		assert.True(t, p.IsVerified())
		assert.Equal(t, int64(7), p.Version())
		assert.Empty(t, p.PullDomainEvents())
	})

	t.Run("rejects corrupt rows", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			name                           string
			id, email, displayName, status string
			version                        int64
			wantErr                        error
		}{
			{"bad id", "garbage", "boris@example.com", "Boris", "registered", 1, participant.ErrInvalidParticipantID},
			{"bad email", validID, "garbage", "Boris", "registered", 1, participant.ErrInvalidEmail},
			{"bad status", validID, "boris@example.com", "Boris", "banned", 1, participant.ErrInvalidStatus},
			{"empty display name", validID, "boris@example.com", " ", "registered", 1, participant.ErrEmptyDisplayName},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				_, err := participant.UnmarshalFromDatabase(tc.id, tc.email, tc.displayName, tc.status, tc.version)
				require.ErrorIs(t, err, tc.wantErr)
			})
		}

		t.Run("version below one", func(t *testing.T) {
			t.Parallel()
			_, err := participant.UnmarshalFromDatabase(validID, "boris@example.com", "Boris", "registered", 0)
			require.Error(t, err)
		})
	})
}
