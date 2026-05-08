package email_test

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/email"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// captureSender records the most recent message instead of sending it.
// Mirrors the pattern in internal/auth/magic_test.go.
type captureSender struct {
	last *email.Message
}

func (c *captureSender) Send(_ context.Context, m email.Message) error {
	cp := m
	c.last = &cp
	return nil
}

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

// seedFixture builds a verified-strategy-backed portfolio and a pending
// proposal, returning everything the mailer needs to send.
func seedFixture(t *testing.T) (
	*proposal.Proposal,
	*users.Repository,
	*portfolio.Repository,
	*proposal.Repository,
	*strategy.Repository,
) {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	ctx := context.Background()

	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	prRepo := proposal.NewRepository(pool)
	usersRepo := users.NewRepository(pool)
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
		Name:              "Quality Compounders",
		StartingCapital:   decimal.NewFromInt(10000),
		StrategyID:        int64(s.ID),
		StrategyVersionID: *got.CurrentVersionID,
		Cadence:           strategy.CadenceQuarterly,
	})
	if err != nil {
		t.Fatalf("seed portfolio: %v", err)
	}

	pr, err := prRepo.Insert(ctx, pool, proposal.InsertInput{
		PortfolioID:           p.ID,
		StrategyVersionID:     *got.CurrentVersionID,
		MarketValueAtProposal: decimal.NewFromInt(10000),
		DeployAmount:          decimal.NewFromInt(10000),
		Picks: []proposal.Pick{
			{Ticker: "AAPL", Action: proposal.ActionBuy,
				TargetWeight: decimal.NewFromInt(1), TargetShares: decimal.NewFromInt(50),
				CurrentShares: decimal.Zero, PriceAtProposal: decimal.NewFromInt(180)},
		},
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return pr, usersRepo, pRepo, prRepo, sRepo
}

func TestSendProposalReady_BuildsLinkAndSubject(t *testing.T) {
	pr, usersRepo, pRepo, prRepo, sRepo := seedFixture(t)
	cs := &captureSender{}

	mailer := email.NewProposalMailer(cs, "noreply@example.com", "https://app.example.com",
		usersRepo, pRepo, prRepo, sRepo)
	if err := mailer.SendProposalReady(context.Background(), pr.ID); err != nil {
		t.Fatalf("send: %v", err)
	}

	if cs.last == nil {
		t.Fatal("captureSender did not receive a message")
	}
	if cs.last.To != "system@deepvalue.local" {
		t.Errorf("To = %q, want system@deepvalue.local", cs.last.To)
	}
	if !strings.Contains(cs.last.Subject, "Quality Compounders") {
		t.Errorf("Subject %q must include portfolio name", cs.last.Subject)
	}
	if !strings.Contains(cs.last.Subject, "rebalance") {
		t.Errorf("Subject %q must mention rebalance", cs.last.Subject)
	}

	wantLink := "https://app.example.com/portfolios/" +
		decimal.NewFromInt(pr.PortfolioID).String() +
		"/proposals/" + decimal.NewFromInt(pr.ID).String()
	if !strings.Contains(cs.last.TextBody, wantLink) {
		t.Errorf("TextBody must contain %q; got %q", wantLink, cs.last.TextBody)
	}
	if !strings.Contains(cs.last.HTMLBody, wantLink) {
		t.Errorf("HTMLBody must contain %q", wantLink)
	}
}

func TestSendProposalReminder_BuildsRemindWording(t *testing.T) {
	pr, usersRepo, pRepo, prRepo, sRepo := seedFixture(t)
	cs := &captureSender{}

	mailer := email.NewProposalMailer(cs, "noreply@example.com", "https://app.example.com",
		usersRepo, pRepo, prRepo, sRepo)
	if err := mailer.SendProposalReminder(context.Background(), pr.ID); err != nil {
		t.Fatalf("send: %v", err)
	}
	if cs.last == nil {
		t.Fatal("captureSender did not receive a message")
	}
	if !strings.Contains(strings.ToLower(cs.last.Subject), "reminder") {
		t.Errorf("Subject %q should mention reminder", cs.last.Subject)
	}
	if !strings.Contains(strings.ToLower(cs.last.TextBody), "still pending") {
		t.Errorf("TextBody should mention 'still pending', got %q", cs.last.TextBody)
	}
}

func TestSendProposalReady_NotFoundProposalReturnsError(t *testing.T) {
	_, usersRepo, pRepo, prRepo, sRepo := seedFixture(t)
	cs := &captureSender{}

	mailer := email.NewProposalMailer(cs, "noreply@example.com", "https://app.example.com",
		usersRepo, pRepo, prRepo, sRepo)
	err := mailer.SendProposalReady(context.Background(), 999_999_999)
	if err == nil {
		t.Error("expected error for missing proposal id")
	}
	if cs.last != nil {
		t.Error("should not have called sender for missing proposal")
	}
}
