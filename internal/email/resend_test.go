package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResendSender_HappyPath(t *testing.T) {
	var captured struct {
		auth string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured.body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	s := NewResendSender("test-key", "from@example.com")
	s.url = srv.URL

	err := s.Send(context.Background(), Message{
		To:       "to@example.com",
		Subject:  "hi",
		HTMLBody: "<p>hi</p>",
		TextBody: "hi",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if captured.auth != "Bearer test-key" {
		t.Errorf("auth header: got %q", captured.auth)
	}
	if got := captured.body["from"]; got != "from@example.com" {
		t.Errorf("from: got %v", got)
	}
	if got, _ := captured.body["to"].([]any); len(got) != 1 || got[0] != "to@example.com" {
		t.Errorf("to: got %v", got)
	}
}

func TestResendSender_ErrorIncludesBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid_from"}`))
	}))
	defer srv.Close()

	s := NewResendSender("test-key", "from@example.com")
	s.url = srv.URL

	err := s.Send(context.Background(), Message{To: "x@example.com", Subject: "s", TextBody: "t"})
	if err == nil || !strings.Contains(err.Error(), "invalid_from") {
		t.Errorf("expected error mentioning body snippet, got %v", err)
	}
}
