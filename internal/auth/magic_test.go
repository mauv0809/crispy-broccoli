package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/email"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// captureSender records messages instead of sending, so handler tests
// can assert on what would have been delivered.
type captureSender struct {
	mu   sync.Mutex
	msgs []email.Message
}

func (c *captureSender) Send(_ context.Context, m email.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *captureSender) take(t *testing.T) email.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.msgs)
		c.mu.Unlock()
		if n > 0 {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.msgs[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("expected an email to be captured within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *captureSender) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// --- DB-backed integration: covers the actual SQL claim semantics.

func TestMagicTokens_ConsumeIsSingleUse(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)
	store := auth.NewMagicTokenStore(pool)

	u, err := repo.Upsert(context.Background(), "alice@example.com", "Alice", false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	hash := []byte("0123456789abcdef0123456789abcdef") // 32 bytes; arbitrary
	if err := store.Insert(context.Background(), hash, u.ID, time.Now().Add(10*time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	gotID, err := store.Consume(context.Background(), hash)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if gotID != u.ID {
		t.Errorf("user_id: got %d, want %d", gotID, u.ID)
	}

	if _, err := store.Consume(context.Background(), hash); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("replay: got %v, want ErrTokenInvalid", err)
	}
}

func TestMagicTokens_ExpiredTokenInvalid(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)
	store := auth.NewMagicTokenStore(pool)

	u, err := repo.Upsert(context.Background(), "alice@example.com", "Alice", false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	hash := []byte("expired-token-hash-xxxxxxxxxxxxx")
	if err := store.Insert(context.Background(), hash, u.ID, time.Now().Add(-1*time.Second)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := store.Consume(context.Background(), hash); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Errorf("expired: got %v, want ErrTokenInvalid", err)
	}
}

func TestMagicTokens_RecentCountWindowed(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := users.NewRepository(pool)
	store := auth.NewMagicTokenStore(pool)

	u, err := repo.Upsert(context.Background(), "alice@example.com", "Alice", false)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 2; i++ {
		hash := []byte("hash-recent-                    ")
		hash[len(hash)-1] = byte('0' + i)
		if err := store.Insert(context.Background(), hash, u.ID, time.Now().Add(10*time.Minute)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	got, err := store.RecentCount(context.Background(), u.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 2 {
		t.Errorf("count: got %d, want 2", got)
	}
}

// --- Handler-level: unknown email + happy path.

func TestRequestHandler_UnknownEmail_NoSendButShowsInbox(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	usersRepo := users.NewRepository(pool)

	sm := newScsForTest(t)
	cap := &captureSender{}
	h := auth.NewMagicHandler(sm, usersRepo, auth.NewMagicTokenStore(pool), cap, "magic@forge.utiger.dk")

	rec := submitMagicRequest(t, h, "ghost@example.com")
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Check your inbox") {
		t.Errorf("body: missing inbox copy, got %q", rec.Body.String())
	}
	// Background goroutine has time to no-op.
	time.Sleep(100 * time.Millisecond)
	if cap.count() != 0 {
		t.Errorf("send count: got %d, want 0 (unknown email must not be emailed)", cap.count())
	}
}

func TestRequestHandler_KnownEmail_SendsLink(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	usersRepo := users.NewRepository(pool)
	if _, err := usersRepo.Upsert(context.Background(), "alice@example.com", "Alice", false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	sm := newScsForTest(t)
	cap := &captureSender{}
	h := auth.NewMagicHandler(sm, usersRepo, auth.NewMagicTokenStore(pool), cap, "magic@forge.utiger.dk")

	rec := submitMagicRequest(t, h, "alice@example.com")
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}

	msg := cap.take(t)
	if msg.To != "alice@example.com" {
		t.Errorf("to: got %q", msg.To)
	}
	if !strings.Contains(msg.TextBody, "/auth/magic/verify?token=") {
		t.Errorf("body must contain verify link, got %q", msg.TextBody)
	}
}

func TestRequestHandler_InvalidEmail_RedirectsToLogin(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	usersRepo := users.NewRepository(pool)
	sm := newScsForTest(t)

	h := auth.NewMagicHandler(sm, usersRepo, auth.NewMagicTokenStore(pool), &captureSender{}, "magic@forge.utiger.dk")

	rec := submitMagicRequest(t, h, "not-an-email")
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/login" {
		t.Errorf("location: got %q, want /auth/login", loc)
	}
}

// --- helpers

func submitMagicRequest(t *testing.T, h *auth.MagicHandler, emailAddr string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"email": {emailAddr}}
	req := httptest.NewRequest(http.MethodPost, "/auth/magic/request", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	if err := h.Request(c); err != nil {
		t.Fatalf("Request: %v", err)
	}
	return rec
}

func newScsForTest(t *testing.T) *scs.SessionManager {
	t.Helper()
	sqlDB, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sm := scs.New()
	sm.Store = postgresstore.New(sqlDB)
	return sm
}
