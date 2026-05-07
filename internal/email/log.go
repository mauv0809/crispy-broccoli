package email

import (
	"context"
	"log/slog"
)

// LogSender prints emails as structured logs instead of sending them.
// Used in dev (when RESEND_API_KEY is unset) and in preview environments
// where we want to test the full auth flow without burning Resend quota
// or risking sends to real inboxes from a PR branch.
type LogSender struct{ Logger *slog.Logger }

func (s LogSender) Send(_ context.Context, msg Message) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("email (LogSender)",
		"to", msg.To,
		"subject", msg.Subject,
		"text", msg.TextBody,
	)
	return nil
}
