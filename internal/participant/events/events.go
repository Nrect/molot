// Package events defines the participant context's public integration
// events — the ONLY participant package other contexts may import.
//
// Events are flat, versioned and append-only: a breaking change means a
// new V2 struct dual-published next to V1, never a field edit. Payloads
// carry primitives and time.Time only; all timestamps are UTC.
package events

import "time"

// Topic is the watermill topic participant integration events are published to.
const Topic = "participant-events"

type ParticipantRegisteredV1 struct {
	EventID       string    `json:"event_id"`
	ParticipantID string    `json:"participant_id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type ParticipantVerifiedV1 struct {
	EventID       string    `json:"event_id"`
	ParticipantID string    `json:"participant_id"`
	OccurredAt    time.Time `json:"occurred_at"`
}
