package participant_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"molot/internal/participant/domain/participant"
)

func TestNewParticipantID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		raw  string
		want string // "" means an ErrInvalidParticipantID is expected
	}{
		{"canonical lower case", "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f", "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f"},
		{"upper case is normalized", "0D6CBD0A-5F2F-4B3F-9C2A-6C1B2A3D4E5F", "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f"},
		{"surrounding spaces are trimmed", "  0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f ", "0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f"},
		{"empty", "", ""},
		{"not a uuid", "not-a-uuid", ""},
		{"too short", "0d6cbd0a-5f2f-4b3f-9c2a", ""},
		{"misplaced dashes", "0d6cbd0a5-f2f-4b3f-9c2a-6c1b2a3d4e5f", ""},
		{"non-hex characters", "0d6cbd0g-5f2f-4b3f-9c2a-6c1b2a3d4e5f", ""},
		{"braced form rejected", "{0d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f}", ""},
		{"nil uuid rejected", "00000000-0000-0000-0000-000000000000", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := participant.NewParticipantID(tc.raw)
			if tc.want == "" {
				require.ErrorIs(t, err, participant.ErrInvalidParticipantID)
				assert.True(t, id.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, id.String())
			assert.False(t, id.IsZero())
		})
	}
}

func TestNewEmailAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		raw  string
		want string // "" means an ErrInvalidEmail is expected
	}{
		{"simple address", "bidder@example.com", "bidder@example.com"},
		{"upper case is normalized", "Bidder@Example.COM", "bidder@example.com"},
		{"surrounding spaces are trimmed", "  bidder@example.com ", "bidder@example.com"},
		{"plus tag", "bidder+auctions@example.com", "bidder+auctions@example.com"},
		{"empty", "", ""},
		{"spaces only", "   ", ""},
		{"missing at sign", "bidder.example.com", ""},
		{"missing local part", "@example.com", ""},
		{"missing domain", "bidder@", ""},
		{"display name form rejected", "Bidder <bidder@example.com>", ""},
		{"inner space rejected", "bid der@example.com", ""},
		{"over RFC length limit", strings.Repeat("a", 310) + "@example.com", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			email, err := participant.NewEmailAddress(tc.raw)
			if tc.want == "" {
				require.ErrorIs(t, err, participant.ErrInvalidEmail)
				assert.True(t, email.IsZero())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, email.String())
			assert.False(t, email.IsZero())
		})
	}
}

func TestNewStatusFromString(t *testing.T) {
	t.Parallel()

	t.Run("round-trips the closed enum", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"registered", "verified"} {
			status, err := participant.NewStatusFromString(raw)
			require.NoError(t, err)
			assert.Equal(t, raw, status.String())
			assert.False(t, status.IsZero())
		}
	})

	t.Run("rejects unknown values", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"", "REGISTERED", "banned"} {
			_, err := participant.NewStatusFromString(raw)
			require.ErrorIs(t, err, participant.ErrInvalidStatus)
		}
	})

	t.Run("String panics outside the closed enum", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() { _ = (participant.Status{}).String() })
	})
}

func TestActor(t *testing.T) {
	t.Parallel()

	t.Run("requires a non-zero id", func(t *testing.T) {
		t.Parallel()
		_, err := participant.NewActor(participant.ParticipantID{})
		require.ErrorIs(t, err, participant.ErrInvalidParticipantID)
	})

	t.Run("owner may update own profile", func(t *testing.T) {
		t.Parallel()
		p := registeredParticipant(t, "owner@example.com")
		actor, err := participant.NewActor(p.ID())
		require.NoError(t, err)

		assert.NoError(t, participant.CanActorUpdateParticipant(actor, *p))
	})

	t.Run("stranger is forbidden with a typed error", func(t *testing.T) {
		t.Parallel()
		p := registeredParticipant(t, "owner@example.com")
		strangerID := mustID(t, "9d6cbd0a-5f2f-4b3f-9c2a-6c1b2a3d4e5f")
		stranger, err := participant.NewActor(strangerID)
		require.NoError(t, err)

		err = participant.CanActorUpdateParticipant(stranger, *p)

		var forbidden participant.ForbiddenParticipantUpdateError
		require.ErrorAs(t, err, &forbidden)
		assert.Equal(t, strangerID, forbidden.Actor)
		assert.Equal(t, p.ID(), forbidden.Target)
	})
}
