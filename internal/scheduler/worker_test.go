package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/scheduler"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
)

// stubMailer records every send call. Optional sendErr forces failures.
type stubMailer struct {
	mu       sync.Mutex
	readyIDs []int64
	remIDs   []int64
	sendErr  error
}

func (m *stubMailer) SendProposalReady(ctx context.Context, proposalID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.readyIDs = append(m.readyIDs, proposalID)
	return nil
}

func (m *stubMailer) SendProposalReminder(ctx context.Context, proposalID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.remIDs = append(m.remIDs, proposalID)
	return nil
}

// stubPickGenerator returns a fixed picks slice regardless of input.
type stubPickGenerator struct {
	picks []proposal.Pick
}

func (s stubPickGenerator) GeneratePicks(ctx context.Context, in proposal.GenerateInput) ([]proposal.Pick, error) {
	// Echo back the request's portfolio context if needed; for tests we just
	// return the seeded picks.
	return s.picks, nil
}

// systemUserID looks up the synthetic system user that OpenTestDB re-inserts.
func systemUserID(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'system@deepvalue.local'`).Scan(&id); err != nil {
		t.Fatalf("system user lookup: %v", err)
	}
	return id
}

// seedPortfolioForScheduler creates a verified-strategy-backed portfolio using
// the provided pool. Callers must pass the pool they obtained from OpenTestDB
// so all operations share a single truncate boundary.
func seedPortfolioForScheduler(t *testing.T, pool *pgxpool.Pool) (*portfolio.Portfolio, int64) {
	t.Helper()
	ctx := context.Background()

	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	uid := systemUserID(t, pool)
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules}, uid)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := sRepo.Verify(ctx, int64(s.ID)); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, _ := sRepo.GetByID(ctx, s.ID)

	p, err := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
		UserID:            uid,
		Name:              t.Name() + "-pf",
		StartingCapital:   decimal.NewFromInt(10000),
		StrategyID:        int64(s.ID),
		StrategyVersionID: *got.CurrentVersionID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("seed portfolio: %v", err)
	}
	return p, *got.CurrentVersionID
}

// newWorker builds a Worker wired to the given pool. All repos share the same
// pool so they see the same data without extra truncation.
func newWorker(t *testing.T, pool *pgxpool.Pool, mailer *stubMailer, picksProvider scheduler.PickGenerator, clock scheduler.Clock) *scheduler.Worker {
	t.Helper()
	return scheduler.NewWorker(scheduler.WorkerConfig{
		Pool:          pool,
		Proposals:     proposal.NewRepository(pool),
		Portfolios:    portfolio.NewRepository(pool),
		Strategies:    strategy.NewRepository(pool),
		Versions:      strategy.NewVersionsRepository(pool),
		PickGenerator: picksProvider,
		Mailer:        mailer,
		Clock:         clock,
		TickInterval:  time.Hour, // unused by RunOnce, but required
		ReminderAfter: 72 * time.Hour,
		RetryWindow:   6 * time.Hour,
	})
}

func TestWorker_RunOnce_GeneratesProposalForDuePortfolio(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, _ := seedPortfolioForScheduler(t, pool)

	// Make the portfolio due.
	past := time.Now().Add(-time.Hour)
	if err := portfolio.NewRepository(pool).SetNextRebalanceDue(ctx, pool, p.ID, past); err != nil {
		t.Fatalf("set due: %v", err)
	}
	// Seed AAPL so the eventual proposal insert (no executed_trades, but
	// generator references prices) doesn't fail. Generator uses a stub here.
	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	mailer := &stubMailer{}
	picks := stubPickGenerator{picks: []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}}
	clock := scheduler.NewFakeClock(time.Now().UTC())

	w := newWorker(t, pool, mailer, picks, clock)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	// A pending proposal now exists for the portfolio.
	pr, err := proposal.NewRepository(pool).GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pr.NotificationSentAt == nil {
		t.Error("notification_sent_at not stamped after successful send")
	}
	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if len(mailer.readyIDs) != 1 || mailer.readyIDs[0] != pr.ID {
		t.Errorf("mailer.readyIDs = %v, want [%d]", mailer.readyIDs, pr.ID)
	}
}

func TestWorker_RunOnce_AutoExpiresPriorPending(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolioForScheduler(t, pool)

	// Seed a pending proposal manually (representing the prior tick's output).
	priorPicks := []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(40),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}
	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}
	prRepo := proposal.NewRepository(pool)
	prior, err := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 priorPicks,
	})
	if err != nil {
		t.Fatalf("seed prior proposal: %v", err)
	}

	// Now make portfolio due so the worker generates again.
	past := time.Now().Add(-time.Hour)
	_ = portfolio.NewRepository(pool).SetNextRebalanceDue(ctx, pool, p.ID, past)

	mailer := &stubMailer{}
	w := newWorker(t, pool, mailer, stubPickGenerator{picks: priorPicks}, scheduler.NewFakeClock(time.Now().UTC()))

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	// Prior proposal should now be expired.
	got, _ := prRepo.Get(ctx, prior.ID)
	if got.Status != proposal.StatusExpired {
		t.Errorf("prior status = %s, want expired", got.Status)
	}
	if got.ResolvedAt == nil {
		t.Error("resolved_at not stamped on expired proposal")
	}
	// And exactly one pending proposal exists (the new one).
	pending, err := prRepo.GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending after run: %v", err)
	}
	if pending.ID == prior.ID {
		t.Errorf("pending proposal is the prior one (%d), should be a new row", prior.ID)
	}
}

func TestWorker_RunOnce_SkipsPausedAndFuturePortfolios(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	// Three portfolios: due+active, due+paused, future+active.
	mkPortfolio := func(name string) *portfolio.Portfolio {
		t.Helper()
		sRepo := strategy.NewRepository(pool)
		pRepo := portfolio.NewRepository(pool)
		uid := systemUserID(t, pool)
		rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
		s, _ := sRepo.Create(ctx, strategy.CreateStrategyRequest{Name: name + "-strat", Rules: rules}, uid)
		_ = sRepo.Verify(ctx, int64(s.ID))
		got, _ := sRepo.GetByID(ctx, s.ID)
		p, _ := pRepo.Create(ctx, portfolio.CreatePortfolioRequest{
			UserID: uid, Name: name,
			StartingCapital: decimal.NewFromInt(10000),
			StrategyID:      int64(s.ID), StrategyVersionID: *got.CurrentVersionID,
			Cadence: strategy.CadenceQuarterly,
		})
		return p
	}

	due := mkPortfolio("due-active")
	paused := mkPortfolio("due-paused")
	future := mkPortfolio("future-active")

	pRepo := portfolio.NewRepository(pool)
	past := time.Now().Add(-time.Hour)
	soon := time.Now().Add(time.Hour)
	_ = pRepo.SetNextRebalanceDue(ctx, pool, due.ID, past)
	_ = pRepo.SetNextRebalanceDue(ctx, pool, paused.ID, past)
	_ = pRepo.SetStatus(ctx, paused.ID, portfolio.StatusPaused)
	_ = pRepo.SetNextRebalanceDue(ctx, pool, future.ID, soon)

	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	mailer := &stubMailer{}
	picks := stubPickGenerator{picks: []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}}
	w := scheduler.NewWorker(scheduler.WorkerConfig{
		Pool: pool, Proposals: proposal.NewRepository(pool), Portfolios: pRepo,
		Strategies: strategy.NewRepository(pool), Versions: strategy.NewVersionsRepository(pool),
		PickGenerator: picks, Mailer: mailer, Clock: scheduler.NewFakeClock(time.Now().UTC()),
		TickInterval: time.Hour, ReminderAfter: 72 * time.Hour, RetryWindow: 6 * time.Hour,
	})

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	prRepo := proposal.NewRepository(pool)

	// Only the due+active portfolio should have a new proposal.
	if _, err := prRepo.GetPending(ctx, pool, due.ID); err != nil {
		t.Errorf("due-active should have pending proposal: %v", err)
	}
	if _, err := prRepo.GetPending(ctx, pool, paused.ID); err != proposal.ErrNotFound {
		t.Errorf("paused should not have pending proposal, err = %v", err)
	}
	if _, err := prRepo.GetPending(ctx, pool, future.ID); err != proposal.ErrNotFound {
		t.Errorf("future should not have pending proposal, err = %v", err)
	}
}

func TestWorker_RunOnce_SendsRemindersForOldNotifications(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolioForScheduler(t, pool)

	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	prRepo := proposal.NewRepository(pool)
	pr, _ := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		},
	})
	// Backdate notification_sent_at by 4 days so it's eligible for reminder.
	if _, err := pool.Exec(ctx,
		`UPDATE proposals SET notification_sent_at = NOW() - INTERVAL '4 days' WHERE id = $1`, pr.ID); err != nil {
		t.Fatalf("backdate notification: %v", err)
	}

	mailer := &stubMailer{}
	w := scheduler.NewWorker(scheduler.WorkerConfig{
		Pool: pool, Proposals: prRepo, Portfolios: portfolio.NewRepository(pool),
		Strategies: strategy.NewRepository(pool), Versions: strategy.NewVersionsRepository(pool),
		PickGenerator: stubPickGenerator{}, Mailer: mailer,
		Clock:        scheduler.NewFakeClock(time.Now().UTC()),
		TickInterval: time.Hour, ReminderAfter: 72 * time.Hour, RetryWindow: 6 * time.Hour,
	})

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if len(mailer.remIDs) != 1 || mailer.remIDs[0] != pr.ID {
		t.Errorf("reminder not sent: remIDs = %v", mailer.remIDs)
	}

	got, _ := prRepo.Get(ctx, pr.ID)
	if got.ReminderSentAt == nil {
		t.Error("reminder_sent_at not stamped")
	}
}

func TestWorker_RunOnce_RetriesUnsentNotifications(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, vID := seedPortfolioForScheduler(t, pool)

	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	prRepo := proposal.NewRepository(pool)
	pr, _ := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		},
	})
	// Backdate generated_at to 10 minutes ago so it lands in the retry window
	// (older than the 5-minute lower bound, younger than 6h upper bound).
	if _, err := pool.Exec(ctx,
		`UPDATE proposals SET generated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, pr.ID); err != nil {
		t.Fatalf("backdate generated_at: %v", err)
	}

	mailer := &stubMailer{}
	w := scheduler.NewWorker(scheduler.WorkerConfig{
		Pool: pool, Proposals: prRepo, Portfolios: portfolio.NewRepository(pool),
		Strategies: strategy.NewRepository(pool), Versions: strategy.NewVersionsRepository(pool),
		PickGenerator: stubPickGenerator{}, Mailer: mailer,
		Clock:        scheduler.NewFakeClock(time.Now().UTC()),
		TickInterval: time.Hour, ReminderAfter: 72 * time.Hour, RetryWindow: 6 * time.Hour,
	})

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if len(mailer.readyIDs) != 1 || mailer.readyIDs[0] != pr.ID {
		t.Errorf("retry send didn't fire: readyIDs = %v", mailer.readyIDs)
	}
	got, _ := prRepo.Get(ctx, pr.ID)
	if got.NotificationSentAt == nil {
		t.Error("notification_sent_at not stamped after retry")
	}
}

func TestWorker_RunOnce_FailedSendLeavesNotificationUnstamped(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	p, _ := seedPortfolioForScheduler(t, pool)
	past := time.Now().Add(-time.Hour)
	_ = portfolio.NewRepository(pool).SetNextRebalanceDue(ctx, pool, p.ID, past)
	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	mailer := &stubMailer{sendErr: errors.New("smtp down")}
	picks := stubPickGenerator{picks: []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}}
	w := newWorker(t, pool, mailer, picks, scheduler.NewFakeClock(time.Now().UTC()))

	// RunOnce returns nil (errors per portfolio are logged, not propagated).
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	pr, err := proposal.NewRepository(pool).GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending after failed send: %v", err)
	}
	if pr.NotificationSentAt != nil {
		t.Errorf("notification_sent_at = %s, should be NULL after failed send", pr.NotificationSentAt)
	}
}

func TestWorker_StartStop_RunsTickAndShutsDown(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, _ := seedPortfolioForScheduler(t, pool)
	past := time.Now().Add(-time.Hour)
	_ = portfolio.NewRepository(pool).SetNextRebalanceDue(ctx, pool, p.ID, past)
	if _, err := pool.Exec(ctx, `INSERT INTO companies (ticker,name,sector,industry,active) VALUES ('AAPL','AAPL','','',true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed ticker: %v", err)
	}

	mailer := &stubMailer{}
	picks := stubPickGenerator{picks: []proposal.Pick{
		{Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
	}}
	w := scheduler.NewWorker(scheduler.WorkerConfig{
		Pool: pool, Proposals: proposal.NewRepository(pool), Portfolios: portfolio.NewRepository(pool),
		Strategies: strategy.NewRepository(pool), Versions: strategy.NewVersionsRepository(pool),
		PickGenerator: picks, Mailer: mailer, Clock: scheduler.NewRealClock(),
		// Tick fast for the test so we don't wait long.
		TickInterval:  50 * time.Millisecond,
		ReminderAfter: 72 * time.Hour,
		RetryWindow:   6 * time.Hour,
	})

	w.Start(ctx)
	// Wait a bit for at least one tick to fire.
	time.Sleep(200 * time.Millisecond)
	w.Stop()

	// After the loop has exited, at least one proposal should have been
	// generated and the mailer called.
	pending, err := proposal.NewRepository(pool).GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.NotificationSentAt == nil {
		t.Error("notification not sent during ticker run")
	}
	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if len(mailer.readyIDs) == 0 {
		t.Error("no email sent during ticker run")
	}
}
