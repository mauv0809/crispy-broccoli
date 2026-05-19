package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// newStrategyHandler builds a handler with the versions repo wired.
// backtester is nil because no test here calls RunBacktest.
func newStrategyHandler(t *testing.T) (*handlers.StrategyHandler, *strategy.Repository, int64) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	repo := strategy.NewRepository(pool)
	executor := strategy.NewExecutor(pool)
	h := handlers.NewStrategyHandler(repo, executor, nil)
	h.SetVersionsRepository(strategy.NewVersionsRepository(pool))
	return h, repo, systemUserID(t, pool)
}

func TestStrategyHandler_Verify_FromDraftSucceeds(t *testing.T) {
	h, repo, uid := newStrategyHandler(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: "VerifyDraft", Rules: rules}, uid)

	req := httptest.NewRequest(http.MethodPost, "/strategies/"+strconv.FormatInt(s.ID, 10)+"/verify", nil)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id")
	c.SetParamValues(strconv.FormatInt(s.ID, 10))

	if err := h.Verify(c); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}

	got, _ := repo.GetByID(context.Background(), s.ID)
	if got.Status != strategy.StatusVerified {
		t.Errorf("status = %s, want verified", got.Status)
	}
}

func TestStrategyHandler_Verify_FromArchivedReturns409(t *testing.T) {
	h, repo, uid := newStrategyHandler(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: "ArchVerify", Rules: rules}, uid)
	_ = repo.Archive(context.Background(), s.ID)

	req := httptest.NewRequest(http.MethodPost, "/strategies/"+strconv.FormatInt(s.ID, 10)+"/verify", nil)
	c, _ := requestWithUser(req, uid, false)
	c.SetParamNames("id")
	c.SetParamValues(strconv.FormatInt(s.ID, 10))

	err := h.Verify(c)
	httpErr, ok := err.(*echo.HTTPError)
	if !ok || httpErr.Code != http.StatusConflict {
		t.Errorf("expected 409, got %v", err)
	}
}

func TestStrategyHandler_Archive_RedirectsAndUpdatesStatus(t *testing.T) {
	h, repo, uid := newStrategyHandler(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: "ArchTest", Rules: rules}, uid)

	req := httptest.NewRequest(http.MethodPost, "/strategies/"+strconv.FormatInt(s.ID, 10)+"/archive", nil)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id")
	c.SetParamValues(strconv.FormatInt(s.ID, 10))

	if err := h.Archive(c); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	got, _ := repo.GetByID(context.Background(), s.ID)
	if got.Status != strategy.StatusArchived {
		t.Errorf("status = %s, want archived", got.Status)
	}
}

func TestStrategyHandler_ListVersions_ReturnsJSONArray(t *testing.T) {
	h, repo, uid := newStrategyHandler(t)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := repo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: "VerList", Rules: rules}, uid)

	req := httptest.NewRequest(http.MethodGet, "/strategies/"+strconv.FormatInt(s.ID, 10)+"/versions", nil)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id")
	c.SetParamValues(strconv.FormatInt(s.ID, 10))

	if err := h.ListVersions(c); err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Errorf("body should be a JSON array, got: %s", body)
	}
	if !strings.Contains(body, "version_number") {
		t.Errorf("body should include version_number field, got: %s", body)
	}
}
