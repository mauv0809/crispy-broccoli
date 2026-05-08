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

// systemUserID looks up the synthetic system user. Mirrors the helper
// used in strategy_test and portfolio_test.
func systemUserID(t *testing.T, pool any) int64 {
	t.Helper()
	p := testutil.PoolFrom(pool)
	var id int64
	err := p.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = 'system@deepvalue.local'`).Scan(&id)
	if err != nil {
		t.Fatalf("system user lookup: %v", err)
	}
	return id
}

// seedPortfolio creates a verified-strategy-backed portfolio for proposal tests.
// Returns (portfolio, strategy_version_id) — both needed by Insert.
func seedPortfolio(t *testing.T, pool any) (*portfolio.Portfolio, int64) {
	t.Helper()
	sRepo := strategy.NewRepository(testutil.PoolFrom(pool))
	pRepo := portfolio.NewRepository(testutil.PoolFrom(pool))
	uid := systemUserID(t, pool)

	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := sRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules}, uid)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := sRepo.Verify(context.Background(), int64(s.ID)); err != nil {
		t.Fatalf("verify strategy: %v", err)
	}
	got, _ := sRepo.GetByID(context.Background(), s.ID)
	if got.CurrentVersionID == nil {
		t.Fatal("strategy CurrentVersionID nil after Create+Verify")
	}

	p, err := pRepo.Create(context.Background(), portfolio.CreatePortfolioRequest{
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

func samplePicks() []proposal.Pick {
	return []proposal.Pick{
		{
			Ticker: "AAPL", Action: proposal.ActionBuy,
			TargetWeight:    decimal.NewFromFloat(0.5),
			TargetShares:    decimal.NewFromInt(20),
			CurrentShares:   decimal.Zero,
			PriceAtProposal: decimal.NewFromInt(180),
		},
	}
}

func TestProposalRepo_InsertAndGet(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, err := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           p.ID,
		StrategyVersionID:     vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if pr.Status != proposal.StatusPending {
		t.Errorf("status = %s, want pending", pr.Status)
	}
	if pr.ID == 0 {
		t.Error("expected non-zero id")
	}

	got, err := repo.Get(ctx, pr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Picks) != 1 || got.Picks[0].Ticker != "AAPL" {
		t.Errorf("picks = %+v, want one AAPL pick", got.Picks)
	}
	if got.Picks[0].Action != proposal.ActionBuy {
		t.Errorf("pick action = %s, want buy", got.Picks[0].Action)
	}
}

func TestProposalRepo_GetMissingReturnsErrNotFound(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	repo := proposal.NewRepository(pool)
	_, err := repo.Get(context.Background(), 999_999_999)
	if err != proposal.ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestProposalRepo_GetPendingReturnsAtMostOne(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	mk := func() *proposal.Proposal {
		pr, err := repo.Insert(ctx, pool, proposal.InsertInput{
			PortfolioID: p.ID, StrategyVersionID: vID,
			MarketValueAtProposal: decimal.NewFromInt(10000),
			CapitalChange:         decimal.Zero,
			DeployAmount:          decimal.NewFromInt(10000),
			Picks:                 samplePicks(),
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return pr
	}

	first := mk()

	pending, err := repo.GetPending(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if pending.ID != first.ID {
		t.Errorf("pending.ID = %d, want %d", pending.ID, first.ID)
	}

	if err := repo.ExpirePending(ctx, pool, p.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}

	gotFirst, _ := repo.Get(ctx, first.ID)
	if gotFirst.Status != proposal.StatusExpired {
		t.Errorf("first.Status = %s, want expired", gotFirst.Status)
	}
	if gotFirst.ResolvedAt == nil {
		t.Error("expected resolved_at to be set after expire")
	}

	// After expiring, GetPending returns ErrNotFound.
	_, err = repo.GetPending(ctx, pool, p.ID)
	if err != proposal.ErrNotFound {
		t.Errorf("get pending after expire = %v, want ErrNotFound", err)
	}

	// A new pending can now be inserted.
	second := mk()
	if second.Status != proposal.StatusPending {
		t.Errorf("second.Status = %s, want pending", second.Status)
	}
}

func TestProposalRepo_UpdatePendingReplacesPicks(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		CapitalChange:         decimal.Zero,
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})

	newPicks := []proposal.Pick{
		{Ticker: "MSFT", Action: proposal.ActionBuy,
			TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(20),
			CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(500)},
	}
	if err := repo.UpdatePending(ctx, pool, pr.ID, proposal.UpdatePendingInput{
		CapitalChange: decimal.NewFromInt(5000),
		DeployAmount:  decimal.NewFromInt(15000),
		Picks:         newPicks,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := repo.Get(ctx, pr.ID)
	if !got.CapitalChange.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("capital_change = %s, want 5000", got.CapitalChange)
	}
	if len(got.Picks) != 1 || got.Picks[0].Ticker != "MSFT" {
		t.Errorf("picks not replaced: %+v", got.Picks)
	}
	if got.Status != proposal.StatusPending {
		t.Errorf("status changed: %s, want pending", got.Status)
	}
}

func TestProposalRepo_UpdatePendingFailsWhenResolved(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})
	if err := repo.MarkResolved(ctx, pool, pr.ID, proposal.StatusAccepted, time.Now().UTC()); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}

	err := repo.UpdatePending(ctx, pool, pr.ID, proposal.UpdatePendingInput{
		CapitalChange: decimal.Zero,
		DeployAmount:  decimal.NewFromInt(10000),
		Picks:         samplePicks(),
	})
	if err == nil {
		t.Error("expected error updating resolved proposal")
	}
}

func TestProposalRepo_MarkResolvedFailsTwice(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})

	if err := repo.MarkResolved(ctx, pool, pr.ID, proposal.StatusAccepted, time.Now().UTC()); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	err := repo.MarkResolved(ctx, pool, pr.ID, proposal.StatusSkipped, time.Now().UTC())
	if err == nil {
		t.Error("expected error resolving twice")
	}
}

func TestProposalRepo_NotificationStamps(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})

	now := time.Now().UTC().Truncate(time.Second)
	if err := repo.SetNotificationSent(ctx, pr.ID, now); err != nil {
		t.Fatalf("set notification: %v", err)
	}
	if err := repo.SetReminderSent(ctx, pr.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("set reminder: %v", err)
	}

	got, _ := repo.Get(ctx, pr.ID)
	if got.NotificationSentAt == nil || !got.NotificationSentAt.Equal(now) {
		t.Errorf("notification_sent_at = %v, want %s", got.NotificationSentAt, now)
	}
	if got.ReminderSentAt == nil || !got.ReminderSentAt.Equal(now.Add(time.Hour)) {
		t.Errorf("reminder_sent_at = %v, want %s", got.ReminderSentAt, now.Add(time.Hour))
	}
}

func TestProposalRepo_FindReminderCandidates(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	old, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})
	// Notification sent 4 days ago, no reminder yet → eligible.
	if _, err := pool.Exec(ctx, `UPDATE proposals SET notification_sent_at = NOW() - INTERVAL '4 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatalf("backdate notification: %v", err)
	}

	// Insert a control: notification sent recently, not eligible.
	_ = repo.ExpirePending(ctx, pool, p.ID) // expire 'old' so we can have another pending
	recent, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})
	_ = repo.SetNotificationSent(ctx, recent.ID, time.Now().UTC())

	// 'old' is now expired; FindReminderCandidates should only surface pending
	// proposals. Verify the filter ignores expired even with old notifications.
	got, err := repo.FindReminderCandidates(ctx, 3*24*time.Hour)
	if err != nil {
		t.Fatalf("find reminders: %v", err)
	}
	for _, p := range got {
		if p.ID == old.ID {
			t.Error("should not surface expired proposals as reminder candidates")
		}
	}
}

func TestProposalRepo_FindUnsentNotifications(t *testing.T) {
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()
	repo := proposal.NewRepository(pool)
	p, vID := seedPortfolio(t, pool)

	pr, _ := repo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID: p.ID, StrategyVersionID: vID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks:                 samplePicks(),
	})
	// Push generated_at 10 minutes back so it falls in the "needs retry" window.
	if _, err := pool.Exec(ctx, `UPDATE proposals SET generated_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, pr.ID); err != nil {
		t.Fatalf("backdate generated_at: %v", err)
	}

	got, err := repo.FindUnsentNotifications(ctx, 6*time.Hour)
	if err != nil {
		t.Fatalf("find unsent: %v", err)
	}
	found := false
	for _, p := range got {
		if p.ID == pr.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected proposal %d in unsent results, got %d candidates", pr.ID, len(got))
	}
}
