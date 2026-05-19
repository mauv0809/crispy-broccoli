package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// stubPickGenerator returns a fixed picks slice. The handler doesn't care
// what's in them; it just needs the generator to succeed.
type stubPickGenerator struct{}

func (stubPickGenerator) GeneratePicks(ctx context.Context, in proposal.GenerateInput) ([]proposal.Pick, error) {
	return []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}, nil
}

// stubMailer ignores sends. We don't assert on email here; ProposalMailer
// has its own test coverage.
type stubMailer struct{}

func (stubMailer) SendProposalReady(ctx context.Context, proposalID int64) error    { return nil }
func (stubMailer) SendProposalReminder(ctx context.Context, proposalID int64) error { return nil }

// systemUserID + the seed helpers.
func systemUserID(t *testing.T, pool any) int64 {
	t.Helper()
	p := testutil.PoolFrom(pool)
	var id int64
	if err := p.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'system@deepvalue.local'`).Scan(&id); err != nil {
		t.Fatalf("system user lookup: %v", err)
	}
	return id
}

func newPortfoliosHandler(t *testing.T) (*handlers.PortfoliosHandler, *strategy.Repository, int64) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	hlds := portfolio.NewHoldings(pool)
	prRepo := proposal.NewRepository(pool)
	versionsRepo := strategy.NewVersionsRepository(pool)
	svc := portfolio.NewService(pRepo, sRepo)
	performance := portfolio.NewPerformance(pool)

	h := handlers.NewPortfoliosHandler(handlers.PortfoliosDeps{
		Pool: pool, Service: svc, Portfolios: pRepo, Holdings: hlds,
		Proposals: prRepo, Strategies: sRepo, Versions: versionsRepo,
		PickGenerator: stubPickGenerator{}, Mailer: stubMailer{},
		Performance: performance,
	})
	return h, sRepo, systemUserID(t, pool)
}

func seedVerifiedStrategy(t *testing.T, sRepo *strategy.Repository, uid int64) *strategy.Strategy {
	t.Helper()
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	// Seed with a default cadence so portfolio.Service.CreatePortfolio doesn't
	// reject calls that omit the cadence form field — most tests don't care
	// about cadence semantics and shouldn't have to remember to set it.
	cadence := strategy.CadenceQuarterly
	s, err := sRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules, DefaultCadence: &cadence}, uid)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := sRepo.Verify(context.Background(), s.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := sRepo.GetByID(context.Background(), s.ID)
	return got
}

// requestWithUser builds an Echo context with a user attached, mimicking
// what RequireAuth middleware does in production.
func requestWithUser(req *http.Request, uid int64, isAdmin bool) (echo.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	e := echo.New()
	c := e.NewContext(req, rec)
	auth.SetUserOnContext(c, &users.User{ID: uid, IsActive: true, IsAdmin: isAdmin})
	return c, rec
}

func TestPortfolios_Create_RedirectsToProposalReview(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	s := seedVerifiedStrategy(t, sRepo, uid)

	form := url.Values{}
	form.Set("name", "Test")
	form.Set("starting_capital", "10000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))
	form.Set("cadence", "quarterly")

	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	c.SetPath("/portfolios")

	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/portfolios/") || !strings.Contains(loc, "/proposals/") {
		t.Errorf("Location = %q, want /portfolios/:id/proposals/:pid", loc)
	}
}

func TestPortfolios_Create_DraftStrategyRendersError(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-draft", Rules: rules}, uid)
	// Don't verify — leave as draft.

	form := url.Values{}
	form.Set("name", "Draft")
	form.Set("starting_capital", "5000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))

	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)

	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "must be verified") {
		t.Error("expected error message about verified strategy in response body")
	}
}

func TestPortfolios_Detail_NonOwnerGets404(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	s := seedVerifiedStrategy(t, sRepo, uid)

	// Owner creates a portfolio.
	form := url.Values{}
	form.Set("name", "Owned")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))
	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	loc := rec.Header().Get("Location")
	// Extract portfolio ID from /portfolios/:id/proposals/:pid.
	parts := strings.Split(loc, "/")
	if len(parts) < 3 {
		t.Fatalf("unexpected location: %q", loc)
	}
	pidStr := parts[2]

	// Different user tries to access it.
	otherReq := httptest.NewRequest(http.MethodGet, "/portfolios/"+pidStr, nil)
	otherC, otherRec := requestWithUser(otherReq, uid+9999, false)
	otherC.SetParamNames("id")
	otherC.SetParamValues(pidStr)

	err := h.Detail(otherC)
	httpErr, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected echo.HTTPError; got %v (rec=%d)", err, otherRec.Code)
	}
	if httpErr.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", httpErr.Code)
	}
}

func TestPortfolios_Detail_AdminCanAccessAnyone(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	s := seedVerifiedStrategy(t, sRepo, uid)

	// Owner creates portfolio.
	form := url.Values{}
	form.Set("name", "Owned-by-uid")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))
	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	loc := rec.Header().Get("Location")
	parts := strings.Split(loc, "/")
	pidStr := parts[2]

	// Admin (different uid, IsAdmin=true) accesses it.
	adminReq := httptest.NewRequest(http.MethodGet, "/portfolios/"+pidStr, nil)
	adminC, adminRec := requestWithUser(adminReq, uid+9999, true)
	adminC.SetParamNames("id")
	adminC.SetParamValues(pidStr)

	if err := h.Detail(adminC); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if adminRec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", adminRec.Code)
	}
}

func TestPortfolios_PauseResume(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	s := seedVerifiedStrategy(t, sRepo, uid)

	form := url.Values{}
	form.Set("name", "PR-test")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))
	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	parts := strings.Split(rec.Header().Get("Location"), "/")
	pidStr := parts[2]

	// Pause
	pauseReq := httptest.NewRequest(http.MethodPost, "/portfolios/"+pidStr+"/pause", nil)
	pauseC, pauseRec := requestWithUser(pauseReq, uid, false)
	pauseC.SetParamNames("id")
	pauseC.SetParamValues(pidStr)
	if err := h.Pause(pauseC); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if pauseRec.Code != http.StatusSeeOther {
		t.Errorf("pause status = %d, want 303", pauseRec.Code)
	}

	// Resume
	resumeReq := httptest.NewRequest(http.MethodPost, "/portfolios/"+pidStr+"/resume", nil)
	resumeC, resumeRec := requestWithUser(resumeReq, uid, false)
	resumeC.SetParamNames("id")
	resumeC.SetParamValues(pidStr)
	if err := h.Resume(resumeC); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumeRec.Code != http.StatusSeeOther {
		t.Errorf("resume status = %d, want 303", resumeRec.Code)
	}
}

func TestPortfolios_Pause_NonOwner404s(t *testing.T) {
	h, sRepo, uid := newPortfoliosHandler(t)
	s := seedVerifiedStrategy(t, sRepo, uid)

	form := url.Values{}
	form.Set("name", "Owned")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", strconv.FormatInt(s.ID, 10))
	req := httptest.NewRequest(http.MethodPost, "/portfolios", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	if err := h.Create(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	parts := strings.Split(rec.Header().Get("Location"), "/")
	pidStr := parts[2]

	otherReq := httptest.NewRequest(http.MethodPost, "/portfolios/"+pidStr+"/pause", nil)
	otherC, _ := requestWithUser(otherReq, uid+9999, false)
	otherC.SetParamNames("id")
	otherC.SetParamValues(pidStr)
	err := h.Pause(otherC)
	httpErr, ok := err.(*echo.HTTPError)
	if !ok || httpErr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %v", err)
	}
}
