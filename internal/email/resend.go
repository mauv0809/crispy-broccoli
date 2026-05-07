package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResendSender posts to https://api.resend.com/emails. We hit the REST
// API directly to avoid pulling in the resend-go SDK for one endpoint.
type ResendSender struct {
	apiKey string
	from   string
	client *http.Client
	url    string // overridable for tests
}

func NewResendSender(apiKey, from string) *ResendSender {
	return &ResendSender{
		apiKey: apiKey,
		from:   from,
		client: &http.Client{Timeout: 10 * time.Second},
		url:    "https://api.resend.com/emails",
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html,omitempty"`
	Text    string   `json:"text,omitempty"`
}

func (s *ResendSender) Send(ctx context.Context, msg Message) error {
	body, err := json.Marshal(resendRequest{
		From:    s.from,
		To:      []string{msg.To},
		Subject: msg.Subject,
		HTML:    msg.HTMLBody,
		Text:    msg.TextBody,
	})
	if err != nil {
		return fmt.Errorf("resend marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("resend req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Read up to 1KB so we surface useful diagnostics without unbounded log spam.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("resend status %d: %s", resp.StatusCode, snippet)
	}
	return nil
}
