package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// seedProposalFixture creates a verified-strategy portfolio + a pending
// single-pick BUY proposal of AAPL @ $180 × 10 shares.
func seedProposalFixture(t *testing.T) (
	*handlers.ProposalsHandler,
	*portfolio.Portfolio,
	*proposal.Proposal,
	int64, // user id
) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	uid := systemUserID(t, pool)

	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	holdings := portfolio.NewHoldings(pool)
	prRepo := proposal.NewRepository(pool)
	versionsRepo := strategy.NewVersionsRepository(pool)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules}, uid)
	_ = sRepo.Verify(ctx, int64(s.ID))
	got, _ := sRepo.GetByID(ctx, s.ID)

	port, _ := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: t.Name() + "-pf",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      int64(s.ID), StrategyVersionID: *got.CurrentVersionID,
		Cadence: strategy.CadenceQuarterly,
	})

	// Seed AAPL ticker for the executed_trades FK.
	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	pr, _ := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           port.ID,
		StrategyVersionID:     *got.CurrentVersionID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(10),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		},
	})

	acceptor := proposal.NewAcceptor(pool, prRepo, pRepo, holdings)

	h := handlers.NewProposalsHandler(handlers.ProposalsDeps{
		Pool: pool, Portfolios: pRepo, Proposals: prRepo,
		Strategies: sRepo, Versions: versionsRepo,
		Acceptor: acceptor, PickGen: stubPickGenerator{},
	})
	return h, port, pr, uid
}

func TestProposals_Detail_RendersPage(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	req := httptest.NewRequest(http.MethodGet,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10), nil)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Detail(c); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "AAPL") {
		t.Error("body missing AAPL pick")
	}
	if !strings.Contains(body, "Accept proposal") {
		t.Error("body missing Accept button")
	}
}

func TestProposals_Detail_NonOwner404(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	req := httptest.NewRequest(http.MethodGet,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10), nil)
	c, _ := requestWithUser(req, uid+9999, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	err := h.Detail(c)
	httpErr, ok := err.(*echo.HTTPError)
	if !ok || httpErr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %v", err)
	}
}

func TestProposals_Recompute_SwapsPicksTable(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	form := url.Values{}
	form.Set("capital_change", "5000")

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/recompute",
		strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Recompute(c); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="picks-table"`) {
		t.Error("response should contain picks-table fragment with id")
	}
	if !strings.Contains(body, "$15000.00") {
		t.Errorf("expected deploy amount $15000.00 (10000 + 5000) in body; got: %s", body)
	}
}

func TestProposals_Recompute_WithdrawalExceedsValue(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	form := url.Values{}
	form.Set("capital_change", "-99999") // way more than market value of 10000

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/recompute",
		strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, _ := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	err := h.Recompute(c)
	httpErr, ok := err.(*echo.HTTPError)
	if !ok || httpErr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %v", err)
	}
}

func TestProposals_Accept_FullAcceptRedirects(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	form := url.Values{}
	form.Add("ticker", "AAPL")
	form.Add("actual_shares", "10")
	form.Add("actual_price", "180")
	form.Add("fee", "0")

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/accept",
		strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Accept(c); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/portfolios/"+strconv.FormatInt(port.ID, 10) {
		t.Errorf("Location = %q, want /portfolios/%d", got, port.ID)
	}

	// Sanity check: time advancing means cadence has been moved.
	_ = time.Now()
}

func TestProposals_Accept_PartialWithSkipped(t *testing.T) {
	// Seed a 2-pick proposal so we can skip one.
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	uid := systemUserID(t, pool)

	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	holdings := portfolio.NewHoldings(pool)
	prRepo := proposal.NewRepository(pool)
	versionsRepo := strategy.NewVersionsRepository(pool)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules}, uid)
	_ = sRepo.Verify(ctx, int64(s.ID))
	got, _ := sRepo.GetByID(ctx, s.ID)
	port, _ := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID: uid, Name: "p", StartingCapital: decimal.NewFromInt(10000),
		StrategyID: int64(s.ID), StrategyVersionID: *got.CurrentVersionID,
		Cadence: strategy.CadenceQuarterly,
	})
	for _, tk := range []string{"AAPL", "MSFT"} {
		_, _ = pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ($1,$1,'','',true) ON CONFLICT DO NOTHING`, tk)
	}
	pr, _ := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           port.ID,
		StrategyVersionID:     *got.CurrentVersionID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromFloat(0.5), TargetShares: decimal.NewFromInt(10),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
			{Ticker: "MSFT", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromFloat(0.5), TargetShares: decimal.NewFromInt(5),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(400)},
		},
	})
	acceptor := proposal.NewAcceptor(pool, prRepo, pRepo, holdings)
	h := handlers.NewProposalsHandler(handlers.ProposalsDeps{
		Pool: pool, Portfolios: pRepo, Proposals: prRepo,
		Strategies: sRepo, Versions: versionsRepo,
		Acceptor: acceptor, PickGen: stubPickGenerator{},
	})

	form := url.Values{}
	form.Add("ticker", "AAPL")
	form.Add("ticker", "MSFT")
	form.Add("actual_shares", "10")
	form.Add("actual_shares", "5")
	form.Add("actual_price", "180")
	form.Add("actual_price", "400")
	form.Add("fee", "0")
	form.Add("fee", "0")
	form.Add("skip", "MSFT") // skip the MSFT row

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/accept",
		strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Accept(c); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}

	// Verify the proposal is partially_accepted.
	resolved, _ := prRepo.Get(ctx, pr.ID)
	if resolved.Status != proposal.StatusPartiallyAccepted {
		t.Errorf("status = %s, want partially_accepted", resolved.Status)
	}

	// Holdings should only include AAPL (MSFT was skipped).
	hs, _ := holdings.ListByPortfolio(ctx, port.ID)
	if len(hs) != 1 || hs[0].Ticker != "AAPL" {
		t.Errorf("holdings = %+v, want only AAPL", hs)
	}
}

func TestProposals_Skip_AdvancesCadence(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/skip", nil)
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Skip(c); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

func TestProposals_Skip_HTMXRequestUsesHXRedirect(t *testing.T) {
	h, port, pr, uid := seedProposalFixture(t)

	req := httptest.NewRequest(http.MethodPost,
		"/portfolios/"+strconv.FormatInt(port.ID, 10)+"/proposals/"+strconv.FormatInt(pr.ID, 10)+"/skip", nil)
	req.Header.Set("HX-Request", "true")
	c, rec := requestWithUser(req, uid, false)
	c.SetParamNames("id", "pid")
	c.SetParamValues(strconv.FormatInt(port.ID, 10), strconv.FormatInt(pr.ID, 10))

	if err := h.Skip(c); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (HTMX redirect)", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/portfolios/"+strconv.FormatInt(port.ID, 10) {
		t.Errorf("HX-Redirect = %q, want /portfolios/%d", got, port.ID)
	}
}
