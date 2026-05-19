package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
	"github.com/mauv0809/crispy-broccoli/internal/observability"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
)

// Mailer is the contract the worker depends on for sending notification and
// reminder emails. Production implements this in internal/email; tests stub it.
type Mailer interface {
	SendProposalReady(ctx context.Context, proposalID int64) error
	SendProposalReminder(ctx context.Context, proposalID int64) error
}

// PickGenerator wraps the pick-generation surface so tests can stub it
// without driving a real strategy executor or daily_prices lookups.
// Production wires *proposal.Generator (via a thin adapter).
type PickGenerator interface {
	GeneratePicks(ctx context.Context, in proposal.GenerateInput) ([]proposal.Pick, error)
}

// WorkerConfig collects the worker's external dependencies. All fields are
// required (the worker doesn't carry sensible defaults — they belong in the
// caller's wiring code).
type WorkerConfig struct {
	Pool          *pgxpool.Pool
	Proposals     *proposal.Repository
	Portfolios    *portfolio.Repository
	Strategies    *strategy.Repository
	Versions      *strategy.VersionsRepository
	PickGenerator PickGenerator
	Mailer        Mailer
	Clock         Clock
	TickInterval  time.Duration // wall clock between ticks
	ReminderAfter time.Duration // age before a 3-day reminder fires
	RetryWindow   time.Duration // age window during which initial-send retries happen
}

// Worker drives the rebalance loop. Start launches a goroutine that ticks
// on a time.Ticker until the context is cancelled or Stop is called. Each
// tick calls RunOnce. Errors per portfolio are logged and counted in
// Sentry; they don't stop the tick.
type Worker struct {
	cfg  WorkerConfig
	stop chan struct{}
	done chan struct{}
}

// NewWorker constructs a Worker from a complete config. Does not start
// anything — call Start when ready.
func NewWorker(cfg WorkerConfig) *Worker {
	return &Worker{cfg: cfg, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the tick goroutine and returns immediately. Safe to call
// once per Worker; calling twice is a programmer error.
func (w *Worker) Start(ctx context.Context) {
	go w.loop(ctx)
}

// Stop signals the worker to exit and blocks until the loop returns. Safe
// to call from any goroutine. After Stop returns, the worker may not be
// restarted.
func (w *Worker) Stop() {
	close(w.stop)
	<-w.done
}

func (w *Worker) loop(ctx context.Context) {
	defer close(w.done)
	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil {
				slog.Error("scheduler tick failed", "err", err)
				observability.CaptureContextError(ctx, err)
			}
		}
	}
}

// RunOnce executes one tick: generate proposals for due portfolios, send
// reminders for stale pending proposals, retry unsent notifications. Errors
// per portfolio or per send are logged but don't stop the tick.
//
// Always returns nil today — the per-portfolio errors are intentionally
// swallowed at the tick boundary so one bad portfolio doesn't disable the
// loop. The signature returns error to leave room for "abort the whole
// tick" failures (DB unreachable, etc.) without breaking callers.
func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.cfg.Clock.Now()

	if err := w.generateDue(ctx, now); err != nil {
		slog.Error("scheduler: generateDue", "err", err)
	}
	if err := w.sendReminders(ctx); err != nil {
		slog.Error("scheduler: sendReminders", "err", err)
	}
	if err := w.retryUnsentNotifications(ctx); err != nil {
		slog.Error("scheduler: retryUnsentNotifications", "err", err)
	}
	return nil
}

const dueBatchSize = 50

func (w *Worker) generateDue(ctx context.Context, now time.Time) error {
	// Find IDs in one transaction (FOR UPDATE SKIP LOCKED inside FindDueForRebalance).
	var ids []int64
	err := dbutil.RunInTx(ctx, w.cfg.Pool, func(tx dbutil.DBTX) error {
		var err error
		ids, err = w.cfg.Portfolios.FindDueForRebalance(ctx, tx, now, dueBatchSize)
		return err
	})
	if err != nil {
		return fmt.Errorf("finding due portfolios: %w", err)
	}

	for _, id := range ids {
		if err := w.generateForPortfolio(ctx, id); err != nil {
			slog.Error("scheduler: generate for portfolio",
				"portfolio_id", id, "err", err)
			observability.CaptureContextError(ctx, err)
		}
	}
	return nil
}

// generateForPortfolio handles one due portfolio: in one transaction, expire
// any prior pending proposal, run the generator, insert the new pending row.
// Then outside the txn, send the email and stamp notification_sent_at on
// success. A failed send leaves notification_sent_at NULL — the retry loop
// in retryUnsentNotifications picks it up on a later tick.
func (w *Worker) generateForPortfolio(ctx context.Context, portfolioID int64) error {
	var newProposalID int64
	err := dbutil.RunInTx(ctx, w.cfg.Pool, func(tx dbutil.DBTX) error {
		// Expire any prior pending proposal so the at-most-one invariant holds.
		if err := w.cfg.Proposals.ExpirePending(ctx, tx, portfolioID); err != nil {
			return fmt.Errorf("expiring prior pending: %w", err)
		}

		port, err := w.cfg.Portfolios.GetByIDTx(ctx, tx, portfolioID)
		if err != nil {
			return fmt.Errorf("loading portfolio: %w", err)
		}
		ver, err := w.cfg.Versions.Get(ctx, port.StrategyVersionID)
		if err != nil {
			return fmt.Errorf("loading strategy version: %w", err)
		}

		marketValue, err := computeMarketValue(ctx, tx, port)
		if err != nil {
			return fmt.Errorf("computing market value: %w", err)
		}

		picks, err := w.cfg.PickGenerator.GeneratePicks(ctx, proposal.GenerateInput{
			PortfolioID:   port.ID,
			Rules:         ver.Rules,
			MarketValue:   marketValue,
			CapitalChange: decimal.Zero,
			StrategyLimit: 0, // 0 = unbounded; strategy's own limit applied upstream
		})
		if err != nil {
			return fmt.Errorf("generating picks: %w", err)
		}

		pr, err := w.cfg.Proposals.Insert(ctx, tx, proposal.InsertInput{
			PortfolioID:           port.ID,
			StrategyVersionID:     port.StrategyVersionID,
			MarketValueAtProposal: marketValue,
			CapitalChange:         decimal.Zero,
			DeployAmount:          marketValue,
			Picks:                 picks,
		})
		if err != nil {
			return fmt.Errorf("inserting proposal: %w", err)
		}
		newProposalID = pr.ID
		return nil
	})
	if err != nil {
		return err
	}

	// Send notification outside the txn so a flaky email service doesn't roll
	// back the proposal.
	if err := w.cfg.Mailer.SendProposalReady(ctx, newProposalID); err != nil {
		// Leave notification_sent_at NULL; retryUnsentNotifications will pick it up.
		slog.Warn("scheduler: send proposal ready failed",
			"proposal_id", newProposalID, "err", err)
		return err
	}
	return w.cfg.Proposals.SetNotificationSent(ctx, newProposalID, w.cfg.Clock.Now())
}

// computeMarketValue returns the portfolio's current market value:
// starting_capital + Σ(capital_events) − Σ(buy spend) + Σ(sell proceeds) − Σ(fees)
// + Σ(holdings shares × latest close).
//
// For an initial portfolio with no executed_trades and no capital_events,
// this equals starting_capital.
func computeMarketValue(ctx context.Context, db dbutil.DBTX, p *portfolio.Portfolio) (decimal.Decimal, error) {
	// Single round trip for cash + holdings value.
	var marketValue decimal.Decimal
	err := db.QueryRow(ctx, `
		WITH cash AS (
			SELECT
				$1::numeric
				+ COALESCE((SELECT SUM(amount) FROM capital_events WHERE portfolio_id = $2), 0)
				+ COALESCE((SELECT SUM(CASE WHEN action='sell' THEN shares*price ELSE -shares*price END) - COALESCE(SUM(fee), 0)
				             FROM executed_trades WHERE portfolio_id = $2), 0) AS amount
		),
		holdings_value AS (
			SELECT COALESCE(SUM(h.shares * dp.close), 0) AS amount
			FROM holdings h
			LEFT JOIN LATERAL (
				SELECT close FROM daily_prices
				WHERE ticker = h.ticker
				ORDER BY date DESC LIMIT 1
			) dp ON true
			WHERE h.portfolio_id = $2
		)
		SELECT (SELECT amount FROM cash) + (SELECT amount FROM holdings_value)
	`, p.StartingCapital, p.ID).Scan(&marketValue)
	if err != nil {
		return decimal.Zero, err
	}
	return marketValue, nil
}

func (w *Worker) sendReminders(ctx context.Context) error {
	candidates, err := w.cfg.Proposals.FindReminderCandidates(ctx, w.cfg.ReminderAfter)
	if err != nil {
		return fmt.Errorf("finding reminder candidates: %w", err)
	}
	for _, pr := range candidates {
		if err := w.cfg.Mailer.SendProposalReminder(ctx, pr.ID); err != nil {
			slog.Warn("scheduler: send reminder failed",
				"proposal_id", pr.ID, "err", err)
			continue
		}
		if err := w.cfg.Proposals.SetReminderSent(ctx, pr.ID, w.cfg.Clock.Now()); err != nil {
			slog.Warn("scheduler: set reminder_sent_at failed",
				"proposal_id", pr.ID, "err", err)
		}
	}
	return nil
}

func (w *Worker) retryUnsentNotifications(ctx context.Context) error {
	candidates, err := w.cfg.Proposals.FindUnsentNotifications(ctx, w.cfg.RetryWindow)
	if err != nil {
		return fmt.Errorf("finding unsent notifications: %w", err)
	}
	for _, pr := range candidates {
		if err := w.cfg.Mailer.SendProposalReady(ctx, pr.ID); err != nil {
			slog.Warn("scheduler: retry initial notification failed",
				"proposal_id", pr.ID, "err", err)
			continue
		}
		if err := w.cfg.Proposals.SetNotificationSent(ctx, pr.ID, w.cfg.Clock.Now()); err != nil {
			slog.Warn("scheduler: set notification_sent_at failed",
				"proposal_id", pr.ID, "err", err)
		}
	}
	return nil
}
