package proposal_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// seedTickerForAcceptor ensures a row exists in companies(ticker) so the
// executed_trades / holdings FK is satisfied. Idempotent.
func seedTickerForAcceptor(t *testing.T, pool *pgxpool.Pool, ticker string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO companies (ticker, name, sector, industry, active)
		 VALUES ($1, $1, '', '', true)
		 ON CONFLICT (ticker) DO NOTHING`,
		ticker)
	if err != nil {
		t.Fatalf("seed ticker %s: %v", ticker, err)
	}
}

// seedPendingProposal builds a fresh portfolio + a single-pick BUY proposal.
func seedPendingProposal(t *testing.T) (*pgxpool.Pool, *portfolio.Portfolio, *proposal.Proposal) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")

	pr, err := proposal.NewRepository(pool).Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           p.ID,
		StrategyVersionID:     vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{
				Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight:    decimal.NewFromInt(1),
				TargetShares:    decimal.NewFromInt(10),
				CurrentShares:   decimal.Zero,
				PriceAtProposal: decimal.NewFromInt(180),
			},
		},
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return pool, p, pr
}

func TestAcceptor_FullAcceptCreatesTradeAndAdvancesCadence(t *testing.T) {
	pool, p, pr := seedPendingProposal(t)
	ctx := context.Background()

	a := proposal.NewAcceptor(pool, proposal.NewRepository(pool),
		portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC().Truncate(time.Second)
	res, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(10),
				ActualPrice: decimal.NewFromInt(180), Fee: decimal.NewFromInt(2)},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusAccepted {
		t.Errorf("status = %s, want accepted", res.Status)
	}

	// Holdings updated with the actual values.
	h, err := portfolio.NewHoldings(pool).Get(ctx, p.ID, "AAPL")
	if err != nil {
		t.Fatalf("get holding: %v", err)
	}
	if !h.Shares.Equal(decimal.NewFromInt(10)) {
		t.Errorf("shares = %s, want 10", h.Shares)
	}
	wantCost := decimal.NewFromInt(180*10 + 2)
	if !h.CostBasis.Equal(wantCost) {
		t.Errorf("cost_basis = %s, want %s", h.CostBasis, wantCost)
	}

	// Trade ledger row inserted.
	var tradeCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM executed_trades WHERE proposal_id = $1`, pr.ID).Scan(&tradeCount); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if tradeCount != 1 {
		t.Errorf("executed_trades count = %d, want 1", tradeCount)
	}

	// Cadence advanced by one quarterly period.
	got, _ := portfolio.NewRepository(pool).GetByID(ctx, p.ID)
	if got.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due nil after accept")
	}
	wantDue := now.AddDate(0, 3, 0)
	if !got.NextRebalanceDue.Equal(wantDue) {
		t.Errorf("next_rebalance_due = %s, want %s", got.NextRebalanceDue, wantDue)
	}

	// Proposal frozen.
	resolved, _ := proposal.NewRepository(pool).Get(ctx, pr.ID)
	if resolved.Status != proposal.StatusAccepted {
		t.Errorf("proposal status = %s, want accepted", resolved.Status)
	}
	if resolved.ResolvedAt == nil {
		t.Error("resolved_at not stamped")
	}
}

func TestAcceptor_PartialAcceptWithSkippedRow(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")
	seedTickerForAcceptor(t, pool, "MSFT")

	repo := proposal.NewRepository(pool)
	pr, err := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
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
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	now := time.Now().UTC()
	res, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(10),
				ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
			{Ticker: "MSFT", Skip: true},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusPartiallyAccepted {
		t.Errorf("status = %s, want partially_accepted", res.Status)
	}

	// Only AAPL should be in holdings; MSFT skipped.
	hs, _ := portfolio.NewHoldings(pool).ListByPortfolio(ctx, p.ID)
	if len(hs) != 1 || hs[0].Ticker != "AAPL" {
		t.Errorf("holdings = %+v, want only AAPL", hs)
	}
}

func TestAcceptor_SkipWholeProposalAdvancesCadence(t *testing.T) {
	pool, p, pr := seedPendingProposal(t)
	ctx := context.Background()

	a := proposal.NewAcceptor(pool, proposal.NewRepository(pool),
		portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC().Truncate(time.Second)
	if err := a.Skip(ctx, pr.ID, now); err != nil {
		t.Fatalf("skip: %v", err)
	}

	resolved, _ := proposal.NewRepository(pool).Get(ctx, pr.ID)
	if resolved.Status != proposal.StatusSkipped {
		t.Errorf("status = %s, want skipped", resolved.Status)
	}

	// No trades.
	var tradeCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM executed_trades WHERE portfolio_id = $1`, p.ID).Scan(&tradeCount)
	if tradeCount != 0 {
		t.Errorf("executed_trades count = %d, want 0 after skip", tradeCount)
	}

	// Cadence still advances.
	got, _ := portfolio.NewRepository(pool).GetByID(ctx, p.ID)
	if got.NextRebalanceDue == nil {
		t.Fatal("next_rebalance_due nil after skip")
	}
	wantDue := now.AddDate(0, 3, 0)
	if !got.NextRebalanceDue.Equal(wantDue) {
		t.Errorf("next_rebalance_due = %s, want %s (advance even on skip)", got.NextRebalanceDue, wantDue)
	}
}

func TestAcceptor_CapitalChangeRecordsEvent(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")

	picks := []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(11),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}
	repo := proposal.NewRepository(pool)
	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.NewFromInt(1000),
		DeployAmount:          decimal.NewFromInt(11000),
		Picks:                 picks,
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	now := time.Now().UTC()
	_, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(11),
				ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	var amount decimal.Decimal
	err = pool.QueryRow(ctx,
		`SELECT amount FROM capital_events WHERE portfolio_id = $1 AND proposal_id = $2`,
		p.ID, pr.ID).Scan(&amount)
	if err != nil {
		t.Fatalf("query capital_event: %v", err)
	}
	if !amount.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("amount = %s, want 1000", amount)
	}
}

func TestAcceptor_AcceptTwiceFails(t *testing.T) {
	pool, _, pr := seedPendingProposal(t)
	ctx := context.Background()

	a := proposal.NewAcceptor(pool, proposal.NewRepository(pool),
		portfolio.NewRepository(pool), portfolio.NewHoldings(pool))

	now := time.Now().UTC()
	_, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(10),
				ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}

	_, err = a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: now,
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualShares: decimal.NewFromInt(10),
				ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err == nil {
		t.Error("expected error on second accept")
	}
}

func TestAcceptor_AddActionDeltaShares(t *testing.T) {
	// Pre-existing AAPL holding of 5; pick says target=15, action=add. The
	// acceptor should record a buy of (15 - 5) = 10 by default.
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")

	// Pre-seed the holding via direct trade insertion + rebuild.
	now0 := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 5, 100, 0, $2)
	`, p.ID, now0); err != nil {
		t.Fatalf("seed prior trade: %v", err)
	}
	if err := portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	repo := proposal.NewRepository(pool)
	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionAdd,
				TargetWeight:    decimal.NewFromInt(1),
				TargetShares:    decimal.NewFromInt(15),
				CurrentShares:   decimal.NewFromInt(5),
				PriceAtProposal: decimal.NewFromInt(180)},
		},
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	// Don't pass ActualShares — let it default to (target - current) = 10.
	_, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: time.Now().UTC(),
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Holding should now be 5 (original) + 10 (delta) = 15.
	h, _ := portfolio.NewHoldings(pool).Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(15)) {
		t.Errorf("shares = %s, want 15", h.Shares)
	}
}

func TestAcceptor_TrimActionDeltaShares(t *testing.T) {
	// Pre-existing AAPL of 20; pick says target=8, action=trim. Default sell
	// delta should be 12.
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")

	now0 := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 20, 100, 0, $2)
	`, p.ID, now0); err != nil {
		t.Fatalf("seed prior trade: %v", err)
	}
	if err := portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	repo := proposal.NewRepository(pool)
	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionTrim,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(8),
				CurrentShares: decimal.NewFromInt(20), PriceAtProposal: decimal.NewFromInt(180)},
		},
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	_, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: time.Now().UTC(),
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL", ActualPrice: decimal.NewFromInt(180), Fee: decimal.Zero},
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	h, _ := portfolio.NewHoldings(pool).Get(ctx, p.ID, "AAPL")
	if !h.Shares.Equal(decimal.NewFromInt(8)) {
		t.Errorf("shares = %s, want 8", h.Shares)
	}
}

func TestAcceptor_HoldRowProducesNoTrade(t *testing.T) {
	// Pre-existing AAPL of 10; pick is hold (target == current). No trade,
	// holding unchanged.
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolio(t, pool)
	seedTickerForAcceptor(t, pool, "AAPL")

	now0 := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := pool.Exec(ctx, `
		INSERT INTO executed_trades (portfolio_id, ticker, action, shares, price, fee, executed_at)
		VALUES ($1, 'AAPL', 'buy', 10, 100, 0, $2)
	`, p.ID, now0); err != nil {
		t.Fatalf("seed prior trade: %v", err)
	}
	if err := portfolio.NewHoldings(pool).Rebuild(ctx, pool, p.ID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	repo := proposal.NewRepository(pool)
	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionHold,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(10),
				CurrentShares: decimal.NewFromInt(10), PriceAtProposal: decimal.NewFromInt(180)},
		},
	})

	a := proposal.NewAcceptor(pool, repo, portfolio.NewRepository(pool), portfolio.NewHoldings(pool))
	res, err := a.Accept(ctx, pr.ID, proposal.AcceptInput{
		Now: time.Now().UTC(),
		Rows: []proposal.RowDecision{
			{Ticker: "AAPL"}, // skip = false but action = hold so no fields needed
		},
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.Status != proposal.StatusAccepted {
		t.Errorf("status = %s, want accepted", res.Status)
	}

	// Only the original prior trade exists; no new trade.
	var tradeCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM executed_trades WHERE proposal_id = $1`, pr.ID).Scan(&tradeCount)
	if tradeCount != 0 {
		t.Errorf("trades for proposal = %d, want 0 (hold produces no trade)", tradeCount)
	}
}
