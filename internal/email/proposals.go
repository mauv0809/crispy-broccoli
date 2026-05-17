package email

import (
	"context"
	"fmt"

	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// ProposalMailer renders and sends rebalance-related emails. It satisfies
// scheduler.Mailer in production.
//
// Bodies are built inline with fmt.Sprintf to match the existing magic-link
// pattern in internal/auth/magic.go — keeping all email construction in one
// idiom across the codebase.
type ProposalMailer struct {
	sender     Sender
	from       string // unused today (Sender pulls from env), kept for future Resend integration
	baseURL    string // e.g. "https://deepvalue.utiger.dk"
	users      *users.Repository
	portfolios *portfolio.Repository
	proposals  *proposal.Repository
	strategies *strategy.Repository
}

// NewProposalMailer wires the dependencies. Production wires this in
// cmd/app/main.go (Phase H); tests wire it directly with a capture-sender.
func NewProposalMailer(
	sender Sender,
	from string,
	baseURL string,
	users *users.Repository,
	portfolios *portfolio.Repository,
	proposals *proposal.Repository,
	strategies *strategy.Repository,
) *ProposalMailer {
	return &ProposalMailer{
		sender: sender, from: from, baseURL: baseURL,
		users: users, portfolios: portfolios, proposals: proposals, strategies: strategies,
	}
}

// SendProposalReady sends the initial "your rebalance is ready" email.
// Loads the proposal, portfolio, user, and strategy needed to render the body,
// then dispatches via the underlying Sender.
func (m *ProposalMailer) SendProposalReady(ctx context.Context, proposalID int64) error {
	pr, port, user, strat, err := m.loadContext(ctx, proposalID)
	if err != nil {
		return err
	}
	link := m.proposalLink(port.ID, pr.ID)
	msg := buildProposalReadyMessage(user, port, strat, link)
	return m.sender.Send(ctx, msg)
}

// SendProposalReminder sends the 3-day-after reminder email. Same load path
// as SendProposalReady; different copy.
func (m *ProposalMailer) SendProposalReminder(ctx context.Context, proposalID int64) error {
	pr, port, user, _, err := m.loadContext(ctx, proposalID)
	if err != nil {
		return err
	}
	link := m.proposalLink(port.ID, pr.ID)
	msg := buildProposalReminderMessage(user, port, pr, link)
	return m.sender.Send(ctx, msg)
}

func (m *ProposalMailer) loadContext(ctx context.Context, proposalID int64) (
	*proposal.Proposal, *portfolio.Portfolio, *users.User, *strategy.Strategy, error,
) {
	pr, err := m.proposals.Get(ctx, proposalID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("loading proposal: %w", err)
	}
	port, err := m.portfolios.GetByID(ctx, pr.PortfolioID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("loading portfolio: %w", err)
	}
	user, err := m.users.GetByID(ctx, port.UserID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("loading user: %w", err)
	}
	strat, err := m.strategies.GetByID(ctx, port.StrategyID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("loading strategy: %w", err)
	}
	return pr, port, user, strat, nil
}

func (m *ProposalMailer) proposalLink(portfolioID, proposalID int64) string {
	return fmt.Sprintf("%s/portfolios/%d/proposals/%d", m.baseURL, portfolioID, proposalID)
}

// displayName returns the user's name or falls back to email. Empty Name
// is treated as "use the email" so we always greet by something.
func displayName(u *users.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// buildProposalReadyMessage renders the initial-notification body. Pure
// function; no DB or context access — easier to read and unit-testable in
// isolation if we ever need to.
func buildProposalReadyMessage(u *users.User, p *portfolio.Portfolio, s *strategy.Strategy, link string) Message {
	subject := fmt.Sprintf("Your %s rebalance is ready", p.Name)

	text := fmt.Sprintf(`Hi %s,

Your portfolio %q is due for a rebalance under the %q strategy.
We've prepared a proposal of which positions to buy, sell, or hold.

Review and confirm:
%s

— DeepValue
`, displayName(u), p.Name, s.Name, link)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#111;line-height:1.5;max-width:560px;margin:0 auto;padding:24px">
<p>Hi %s,</p>
<p>Your portfolio <strong>%s</strong> is due for a rebalance under the <strong>%s</strong> strategy. We've prepared a proposal of which positions to buy, sell, or hold.</p>
<p><a href="%s" style="display:inline-block;background:#2563eb;color:#fff;padding:10px 16px;border-radius:6px;text-decoration:none">Review proposal</a></p>
<p style="color:#6b7280;font-size:13px">— DeepValue</p>
</body></html>
`, displayName(u), p.Name, s.Name, link)

	return Message{
		To:       u.Email,
		Subject:  subject,
		HTMLBody: html,
		TextBody: text,
	}
}

// buildProposalReminderMessage renders the 3-day reminder. Same shape as
// the initial notification but with different copy.
func buildProposalReminderMessage(u *users.User, p *portfolio.Portfolio, pr *proposal.Proposal, link string) Message {
	subject := fmt.Sprintf("Reminder: your %s rebalance still needs your review", p.Name)
	generated := pr.GeneratedAt.Format("January 2, 2006")

	text := fmt.Sprintf(`Hi %s,

Your rebalance proposal from %s for %q is still pending.

Take a look (or skip it if you'd rather):
%s

— DeepValue
`, displayName(u), generated, p.Name, link)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#111;line-height:1.5;max-width:560px;margin:0 auto;padding:24px">
<p>Hi %s,</p>
<p>Your rebalance proposal from <strong>%s</strong> for <strong>%s</strong> is still pending.</p>
<p><a href="%s" style="display:inline-block;background:#2563eb;color:#fff;padding:10px 16px;border-radius:6px;text-decoration:none">Take a look</a></p>
<p style="color:#6b7280;font-size:13px">If you'd rather skip this rebalance, you can do that from the same page.</p>
<p style="color:#6b7280;font-size:13px">— DeepValue</p>
</body></html>
`, displayName(u), generated, p.Name, link)

	return Message{
		To:       u.Email,
		Subject:  subject,
		HTMLBody: html,
		TextBody: text,
	}
}
