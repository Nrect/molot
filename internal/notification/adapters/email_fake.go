// Package adapters holds the infrastructure of the notification
// context: the fake email sender (structured slog output), the
// Postgres and in-memory implementations of the ports interfaces, and
// the embedded goose migrations of the notification schema.
package adapters

import (
	"context"
	"log/slog"
)

// FakeEmailSender is the local-stack EmailSender: it "delivers" by
// logging the full email structurally (to/subject/body), so component
// and e2e runs can assert on log output without an SMTP dependency.
type FakeEmailSender struct {
	logger *slog.Logger
}

// NewFakeEmailSender panics on a nil logger: composition-time failure
// beats a nil dereference on the first notification.
func NewFakeEmailSender(logger *slog.Logger) *FakeEmailSender {
	if logger == nil {
		panic("notification.NewFakeEmailSender: nil logger")
	}
	return &FakeEmailSender{logger: logger}
}

// Send logs the email; it never fails, which keeps the
// at-most-once-after-dedup window (README) theoretical for the fake.
func (s *FakeEmailSender) Send(ctx context.Context, to, subject, body string) error {
	s.logger.InfoContext(ctx, "email sent",
		slog.String("to", to),
		slog.String("subject", subject),
		slog.String("body", body),
	)
	return nil
}
