package proposal_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// TestEndToEnd_CreateProposeAcceptRebalance walks the full advisory loop:
//
//  1. Seed a verified strategy with a default cadence.
//  2. Use portfolio.Service.CreatePortfolio (the real service path the
//     handler uses) to create a portfolio with starting_capital.
//  3. Insert a proposal directly with two BUY picks. We bypass the real
//     pick generator here because driving the strategy executor would need
//     financial_metrics + universe data — out of scope for this test, which
//     focuses on the acceptor's downstream effects across all touched
//     tables.
//  4. Accept the proposal with one row edited (different fee) and one row
//     skipped, mimicking a user who actually executed AAPL but never got
//     around to MSFT.
//  5. Assert across every touched table: executed_trades shape, holdings
//     projection, capital_events count, proposal status + resolved_at,
//     next_rebalance_due, and projection-vs-ledger consistency via Rebuild.
//
// If anything in the integration drifts (service stops pinning the version,
// acceptor stops advancing cadence, holdings projection disagrees with the
// ledger) this test fails loudly.
func TestEndToEnd_CreateProposeAcceptRebalance(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	uid := systemUserID(t, pool)
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	prRepo := proposal.NewRepository(pool)
	holdings := portfolio.NewHoldings(pool)
	svc := portfolio.NewService(pRepo, sRepo)

	// 1. Verified strategy with a default cadence.
	cadence := strategy.CadenceQuarterly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := sRepo.Create(ctx,
		strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules, DefaultCadence: &cadence},
		uid)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := sRepo.Verify(ctx, int64(s.ID)); err != nil {
		t.Fatalf("verify strategy: %v", err)
	}

	for _, tk := range []string{"AAPL", "MSFT"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO companies (ticker,name,sector,industry,active)
			 VALUES ($1,$1,'','',true) ON CONFLICT DO NOTHING`, tk); err != nil {
			t.Fatalf("seed ticker %s: %v", tk, err)
		}
	}

	// 2. Create the portfolio via the service so we exercise the
	// strategy-must-be-verified guard + version pinning + cadence resolution.
	port, err := svc.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID:          uid,
		Name:            "E2E",
		StartingCapital: decimal.NewFromInt(10000),
		StrategyID:      int64(s.ID),
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	if port.StrategyVersionID == 0 {
		t.Fatal("StrategyVersionID not pinned")
	}
	if port.Cadence != strategy.CadenceQuarterly {
		t.Errorf("cadence = %s, want quarterly (from strategy default)", port.Cadence)
	}

	// 3. Insert a proposal directly. Two BUY picks: AAPL 27 shares @ $180,
	// MSFT 12 shares @ $400.
	pr, err := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           port.ID,
		StrategyVersionID:     port.StrategyVersionID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromFloat(0.5), TargetShares: decimal.NewFromInt(27),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
			{Ticker: "MSFT", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromFloat(0.5), TargetShares: decimal.NewFromInt(12),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(400)},
		},
	})
	if err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	// 4. Accept: AAPL with edited fee ($2), MSFT skipped.
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	acceptor := proposal.NewAcceptor(pool, prRepo, pRepo, holdings)
	res, err := acceptor.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(27), ActualPrice: decimal.NewFromInt(180), Fee: decimal.NewFromInt(2)},
			{Ticker: "MSFT", Skip: true},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusPartiallyAccepted {
		t.Errorf("res.Status = %s, want partially_accepted", res.Status)
	}

	// 5a. executed_trades: exactly one row, for AAPL, with the user-supplied fee.
	rows, err := pool.Query(ctx, `
		SELECT ticker, action, shares, price, fee
		FROM executed_trades
		WHERE portfolio_id = $1
		ORDER BY id ASC
	`, port.ID)
	if err != nil {
		t.Fatalf("query trades: %v", err)
	}
	defer rows.Close()
	type tradeRow struct {
		Ticker string
		Action string
		Shares decimal.Decimal
		Price  decimal.Decimal
		Fee    decimal.Decimal
	}
	var trades []tradeRow
	for rows.Next() {
		var tr tradeRow
		if err := rows.Scan(&tr.Ticker, &tr.Action, &tr.Shares, &tr.Price, &tr.Fee); err != nil {
			t.Fatalf("scan trade: %v", err)
		}
		trades = append(trades, tr)
	}
	if len(trades) != 1 {
		t.Fatalf("len(trades) = %d, want 1 (AAPL only; MSFT was skipped)", len(trades))
	}
	if trades[0].Ticker != "AAPL" || trades[0].Action != "buy" {
		t.Errorf("trade = %+v, want AAPL buy", trades[0])
	}
	if !trades[0].Shares.Equal(decimal.NewFromInt(27)) {
		t.Errorf("shares = %s, want 27", trades[0].Shares)
	}
	if !trades[0].Fee.Equal(decimal.NewFromInt(2)) {
		t.Errorf("fee = %s, want 2", trades[0].Fee)
	}

	// 5b. holdings: exactly AAPL with cost basis 27*180 + 2 = 4862.
	hs, err := holdings.ListByPortfolio(ctx, port.ID)
	if err != nil {
		t.Fatalf("list holdings: %v", err)
	}
	if len(hs) != 1 || hs[0].Ticker != "AAPL" {
		t.Fatalf("holdings = %+v, want only AAPL", hs)
	}
	wantCost := decimal.NewFromInt(27*180 + 2)
	if !hs[0].CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", hs[0].CostBasis, wantCost)
	}

	// 5c. capital_events: none (capital_change was zero).
	var capCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM capital_events WHERE portfolio_id = $1`,
		port.ID).Scan(&capCount); err != nil {
		t.Fatalf("count capital_events: %v", err)
	}
	if capCount != 0 {
		t.Errorf("capital_events count = %d, want 0", capCount)
	}

	// 5d. proposal frozen as partially_accepted with resolved_at == now.
	resolved, err := prRepo.Get(ctx, pr.ID)
	if err != nil {
		t.Fatalf("get resolved proposal: %v", err)
	}
	if resolved.Status != proposal.StatusPartiallyAccepted {
		t.Errorf("proposal status = %s, want partially_accepted", resolved.Status)
	}
	if resolved.ResolvedAt == nil || !resolved.ResolvedAt.Equal(now) {
		t.Errorf("resolved_at = %v, want %s", resolved.ResolvedAt, now)
	}

	// 5e. next_rebalance_due == now + 3 months (quarterly cadence).
	got, err := pRepo.GetByID(ctx, port.ID)
	if err != nil {
		t.Fatalf("get portfolio: %v", err)
	}
	if got.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due is nil")
	}
	wantDue := now.AddDate(0, 3, 0)
	if !got.NextRebalanceDue.Equal(wantDue) {
		t.Errorf("next_rebalance_due = %s, want %s", got.NextRebalanceDue, wantDue)
	}

	// 5f. Sanity: holdings.Rebuild produces the same projection (proves the
	// projection is consistent with the trade ledger).
	if err := holdings.Rebuild(ctx, pool, port.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	hs2, _ := holdings.ListByPortfolio(ctx, port.ID)
	if len(hs2) != 1 || !hs2[0].Shares.Equal(hs[0].Shares) || !hs2[0].CostBasis.Equal(hs[0].CostBasis) {
		t.Errorf("rebuild diverged: before=%+v after=%+v", hs, hs2)
	}
}
