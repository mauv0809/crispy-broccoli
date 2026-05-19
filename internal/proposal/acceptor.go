package proposal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
)

// Acceptor is the transactional commit point of the rebalance loop. It takes
// a pending proposal + per-row decisions and emits executed_trades, an
// optional capital_event, and updated holdings — then marks the proposal
// resolved and advances the portfolio's next_rebalance_due. Everything runs
// in a single transaction; any error rolls back the whole thing.
type Acceptor struct {
	pool       *pgxpool.Pool
	proposals  *Repository
	portfolios *portfolio.Repository
	holdings   *portfolio.Holdings
}

func NewAcceptor(pool *pgxpool.Pool, pr *Repository, pf *portfolio.Repository, h *portfolio.Holdings) *Acceptor {
	return &Acceptor{pool: pool, proposals: pr, portfolios: pf, holdings: h}
}

// RowDecision is the user's choice for one pick. Skip=true means no trade is
// recorded for this row (off-strategy drift). Otherwise ActualShares /
// ActualPrice / Fee are user-supplied; defaults (when unset / zero) come
// from the pick's target_shares / price_at_proposal / 0.
type RowDecision struct {
	Ticker       string
	Skip         bool
	ActualShares decimal.Decimal // optional; defaults to the pick's natural delta
	ActualPrice  decimal.Decimal // optional; defaults to PriceAtProposal
	Fee          decimal.Decimal // defaults to 0
}

// AcceptInput is the full input to Accept. Now is injected so tests can use a
// fixed clock; production handlers pass time.Now().UTC().
type AcceptInput struct {
	Now  time.Time
	Rows []RowDecision
}

// AcceptResult reports the outcome — primarily the resolved status, which
// the handler uses to render a success page or partial-acceptance summary.
type AcceptResult struct {
	ProposalID int64
	Status     Status
}

// Accept commits the rebalance for a pending proposal. Single transaction;
// rolls back on any error. See package doc for the full state-transition
// model.
func (a *Acceptor) Accept(ctx context.Context, proposalID int64, in AcceptInput) (*AcceptResult, error) {
	var result AcceptResult
	err := dbutil.RunInTx(ctx, a.pool, func(tx dbutil.DBTX) error {
		pr, err := a.proposals.GetTx(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if pr.Status != StatusPending {
			return fmt.Errorf("cannot accept proposal in status %s", pr.Status)
		}

		port, err := a.portfolios.GetByIDTx(ctx, tx, pr.PortfolioID)
		if err != nil {
			return err
		}

		decisions := make(map[string]RowDecision, len(in.Rows))
		for _, d := range in.Rows {
			decisions[d.Ticker] = d
		}

		anySkipped := false
		for _, p := range pr.Picks {
			d, ok := decisions[p.Ticker]
			if !ok {
				return fmt.Errorf("missing decision for pick %s", p.Ticker)
			}
			if d.Skip {
				anySkipped = true
				continue
			}
			if p.Action == ActionHold {
				continue // hold rows produce no trade
			}

			tradeAction, defaultShares := normalizeAction(p)
			if tradeAction == "" {
				return fmt.Errorf("unknown pick action %q for %s", p.Action, p.Ticker)
			}
			shares := d.ActualShares
			if shares.IsZero() {
				shares = defaultShares
			}
			if shares.IsZero() || shares.IsNegative() {
				return fmt.Errorf("invalid trade shares for %s: %s", p.Ticker, shares)
			}
			price := d.ActualPrice
			if price.IsZero() {
				price = p.PriceAtProposal
			}

			// Append to executed_trades ledger.
			if _, err := tx.Exec(ctx, `
				INSERT INTO executed_trades
				    (portfolio_id, proposal_id, ticker, action, shares, price, fee, executed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`, port.ID, pr.ID, p.Ticker, tradeAction, shares, price, d.Fee, in.Now); err != nil {
				return fmt.Errorf("inserting executed_trade for %s: %w", p.Ticker, err)
			}

			// Apply to holdings projection.
			if err := a.holdings.ApplyTrade(ctx, tx, portfolio.TradeApplication{
				PortfolioID: port.ID,
				Ticker:      p.Ticker,
				Action:      tradeAction,
				Shares:      shares,
				Price:       price,
				Fee:         d.Fee,
				ExecutedAt:  in.Now,
			}); err != nil {
				return fmt.Errorf("applying trade for %s: %w", p.Ticker, err)
			}
		}

		// Capital event (if any).
		if !pr.CapitalChange.IsZero() {
			if _, err := tx.Exec(ctx, `
				INSERT INTO capital_events (portfolio_id, proposal_id, amount, occurred_at)
				VALUES ($1, $2, $3, $4)
			`, port.ID, pr.ID, pr.CapitalChange, in.Now); err != nil {
				return fmt.Errorf("inserting capital_event: %w", err)
			}
		}

		// Resolve proposal.
		status := StatusAccepted
		if anySkipped {
			status = StatusPartiallyAccepted
		}
		if err := a.proposals.MarkResolved(ctx, tx, pr.ID, status, in.Now); err != nil {
			return err
		}

		// Advance cadence.
		due, err := AddCadence(in.Now, port.Cadence)
		if err != nil {
			return fmt.Errorf("computing next due date: %w", err)
		}
		if err := a.portfolios.SetNextRebalanceDue(ctx, tx, port.ID, due); err != nil {
			return err
		}

		result.ProposalID = pr.ID
		result.Status = status
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Skip marks the proposal as skipped and advances cadence by one period.
// No trades, no capital event, no holdings change.
func (a *Acceptor) Skip(ctx context.Context, proposalID int64, now time.Time) error {
	return dbutil.RunInTx(ctx, a.pool, func(tx dbutil.DBTX) error {
		pr, err := a.proposals.GetTx(ctx, tx, proposalID)
		if err != nil {
			return err
		}
		if pr.Status != StatusPending {
			return fmt.Errorf("cannot skip proposal in status %s", pr.Status)
		}
		port, err := a.portfolios.GetByIDTx(ctx, tx, pr.PortfolioID)
		if err != nil {
			return err
		}
		if err := a.proposals.MarkResolved(ctx, tx, pr.ID, StatusSkipped, now); err != nil {
			return err
		}
		due, err := AddCadence(now, port.Cadence)
		if err != nil {
			return fmt.Errorf("computing next due date: %w", err)
		}
		return a.portfolios.SetNextRebalanceDue(ctx, tx, port.ID, due)
	})
}

// normalizeAction maps a pick's labeled action + target/current shares to the
// (executed_trade.action, default delta shares) pair. Returns ("", 0) for
// labels that don't produce a trade (Hold) or unknown labels.
//
// Defaults — overridable by RowDecision.ActualShares:
//   - buy:  shares = target_shares       (action = "buy")
//   - add:  shares = target − current    (action = "buy")
//   - sell: shares = current_shares      (action = "sell")
//   - trim: shares = current − target    (action = "sell")
//   - hold: no trade
func normalizeAction(p Pick) (string, decimal.Decimal) {
	switch p.Action {
	case ActionBuy:
		return "buy", p.TargetShares
	case ActionAdd:
		return "buy", p.TargetShares.Sub(p.CurrentShares)
	case ActionSell:
		return "sell", p.CurrentShares
	case ActionTrim:
		return "sell", p.CurrentShares.Sub(p.TargetShares)
	}
	return "", decimal.Zero
}

// ErrNotPending is returned when Accept or Skip is called on a proposal that
// is not in pending status.
var ErrNotPending = errors.New("proposal is not pending")
