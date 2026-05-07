package email_test

import (
	"context"
	"testing"

	"github.com/mauv0809/crispy-broccoli/internal/email"
)

func TestLogSender_AlwaysSucceeds(t *testing.T) {
	var s email.Sender = email.LogSender{}
	if err := s.Send(context.Background(), email.Message{To: "x@example.com", Subject: "s", TextBody: "t"}); err != nil {
		t.Errorf("LogSender.Send returned %v", err)
	}
}
