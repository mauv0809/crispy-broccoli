package portfolio

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Performance computes portfolio-level analytics: a current snapshot today,
// daily time-series replay (Phase I2). All reads — never writes; safe to use
// from any goroutine that has a pool reference.
type Performance struct {
	pool *pgxpool.Pool
}

// NewPerformance wires the pool. Production wires this in cmd/app/main.go;
// the portfolios handler calls Current to render the value/return summary.
func NewPerformance(pool *pgxpool.Pool) *Performance {
	return &Performance{pool: pool}
}

// Snapshot is a point-in-time view of a portfolio's value and return. The
// six numeric fields satisfy the canonical "what's it worth, how much have
// I put in, what's the return" question. AsOf is set to time.Now() at
// computation time — useful for cache invalidation later.
type Snapshot struct {
	PortfolioID   int64
	AsOf          time.Time
	Cash          decimal.Decimal
	HoldingsValue decimal.Decimal
	MarketValue   decimal.Decimal // = Cash + HoldingsValue
	NetInvested   decimal.Decimal // starting_capital + Σ(capital_events.amount)
	ReturnAmount  decimal.Decimal // MarketValue − NetInvested
	ReturnPct     decimal.Decimal // ReturnAmount / NetInvested × 100, rounded(2)
}

// Current computes the snapshot in one Postgres round-trip. Holdings are
// valued at the most recent close from daily_prices. Tickers without price
// data contribute 0 to HoldingsValue (silently — production would surface
// this somewhere visible, but the v1 UX is that "no price = treat as zero
// position" since an unpriced ticker is itself a data-quality issue).
func (p *Performance) Current(ctx context.Context, portfolioID int64) (*Snapshot, error) {
	const q = `
WITH params AS (
    SELECT
        p.id,
        p.starting_capital
    FROM portfolios p
    WHERE p.id = $1
),
capital AS (
    SELECT COALESCE(SUM(amount), 0) AS amount
    FROM capital_events
    WHERE portfolio_id = $1
),
trade_flow AS (
    SELECT
        COALESCE(SUM(CASE WHEN action='sell' THEN shares*price ELSE -shares*price END), 0) AS net,
        COALESCE(SUM(fee), 0) AS fees
    FROM executed_trades
    WHERE portfolio_id = $1
),
holdings_value AS (
    SELECT COALESCE(SUM(h.shares * dp.close), 0) AS amount
    FROM holdings h
    LEFT JOIN LATERAL (
        SELECT close FROM daily_prices
        WHERE ticker = h.ticker
        ORDER BY date DESC LIMIT 1
    ) dp ON true
    WHERE h.portfolio_id = $1
)
SELECT
    p.starting_capital + c.amount + t.net - t.fees      AS cash,
    h.amount                                            AS holdings_value,
    p.starting_capital + c.amount                       AS net_invested
FROM params p, capital c, trade_flow t, holdings_value h
`
	var snap Snapshot
	snap.PortfolioID = portfolioID
	snap.AsOf = time.Now().UTC()

	err := p.pool.QueryRow(ctx, q, portfolioID).Scan(
		&snap.Cash, &snap.HoldingsValue, &snap.NetInvested,
	)
	if err != nil {
		return nil, fmt.Errorf("computing portfolio snapshot: %w", err)
	}
	snap.MarketValue = snap.Cash.Add(snap.HoldingsValue)
	snap.ReturnAmount = snap.MarketValue.Sub(snap.NetInvested)
	if snap.NetInvested.IsZero() {
		snap.ReturnPct = decimal.Zero
	} else {
		snap.ReturnPct = snap.ReturnAmount.Div(snap.NetInvested).
			Mul(decimal.NewFromInt(100)).Round(2)
	}
	return &snap, nil
}

// TimePoint is one day in a TimeSeries.
type TimePoint struct {
	Date                time.Time       `json:"date"`
	PortfolioValue      decimal.Decimal `json:"portfolio_value"`
	PortfolioNormalised decimal.Decimal `json:"portfolio_normalised"`
	SPYNormalised       decimal.Decimal `json:"spy_normalised"`
}

// TimeSeries is the chartable portfolio history.
type TimeSeries struct {
	Points []TimePoint `json:"points"`
}

// TimeSeries replays trades + capital events day-by-day from `from` to `to`
// inclusive (both UTC, day-aligned). Holdings are valued at each day's
// close from daily_prices; missing days reuse the most recent prior close
// so weekends and gaps don't cause artificial dips. SPY normalised series
// is anchored at the first day with a non-zero SPY close.
//
// Cheap because the trade ledger is sparse — typically a few rows per
// quarter per portfolio. Day iteration is O(days × tickers); for v1
// horizons (months to a year) and ~6 tickers per portfolio that's tiny.
func (p *Performance) TimeSeries(ctx context.Context, portfolioID int64, from, to time.Time) (*TimeSeries, error) {
	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)

	startingCapital, err := p.getStartingCapital(ctx, portfolioID)
	if err != nil {
		return nil, err
	}

	trades, err := p.loadTrades(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	caps, err := p.loadCapitalEvents(ctx, portfolioID)
	if err != nil {
		return nil, err
	}

	// Pre-load all daily_prices for tickers we'll touch + benchmark prices for SPY.
	tickers := tickersFromTrades(trades)
	priceLookup, err := p.loadPriceLookup(ctx, tickers, from, to)
	if err != nil {
		return nil, err
	}
	spyLookup, err := p.loadSPYLookup(ctx, from, to)
	if err != nil {
		return nil, err
	}

	holdings := map[string]decimal.Decimal{}
	cash := startingCapital

	// Pre-apply any events strictly before `from` so the series starts in the
	// right state.
	for _, t := range trades {
		if !t.date.Before(from) {
			break
		}
		applyTrade(holdings, &cash, t)
	}
	for _, c := range caps {
		if !c.date.Before(from) {
			break
		}
		cash = cash.Add(c.amount)
	}
	tradeIdx := indexAfter(len(trades), func(i int) bool { return !trades[i].date.Before(from) })
	capIdx := indexAfter(len(caps), func(i int) bool { return !caps[i].date.Before(from) })

	out := &TimeSeries{}
	var portfolioAnchor, spyAnchor decimal.Decimal

	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		// Apply events with date == d.
		for tradeIdx < len(trades) && sameDay(trades[tradeIdx].date, d) {
			applyTrade(holdings, &cash, trades[tradeIdx])
			tradeIdx++
		}
		for capIdx < len(caps) && sameDay(caps[capIdx].date, d) {
			cash = cash.Add(caps[capIdx].amount)
			capIdx++
		}

		// Value holdings at d's close (or last-seen).
		var holdingsValue decimal.Decimal
		for ticker, shares := range holdings {
			if shares.IsZero() {
				continue
			}
			price, ok := priceLookup.LastSeen(ticker, d)
			if !ok {
				continue
			}
			holdingsValue = holdingsValue.Add(shares.Mul(price))
		}
		value := cash.Add(holdingsValue)

		spyClose, _ := spyLookup.LastSeen("SPY", d)

		if portfolioAnchor.IsZero() && !value.IsZero() {
			portfolioAnchor = value
		}
		if spyAnchor.IsZero() && !spyClose.IsZero() {
			spyAnchor = spyClose
		}

		var pn, sn decimal.Decimal
		if !portfolioAnchor.IsZero() {
			pn = value.Div(portfolioAnchor).Round(6)
		}
		if !spyAnchor.IsZero() && !spyClose.IsZero() {
			sn = spyClose.Div(spyAnchor).Round(6)
		}

		out.Points = append(out.Points, TimePoint{
			Date: d, PortfolioValue: value,
			PortfolioNormalised: pn, SPYNormalised: sn,
		})
	}
	return out, nil
}

// --- Internal helpers below ---

type tradeEvent struct {
	date   time.Time
	ticker string
	action string
	shares decimal.Decimal
	price  decimal.Decimal
	fee    decimal.Decimal
}

type capEvent struct {
	date   time.Time
	amount decimal.Decimal
}

func (p *Performance) getStartingCapital(ctx context.Context, portfolioID int64) (decimal.Decimal, error) {
	var startingCapital decimal.Decimal
	err := p.pool.QueryRow(ctx,
		`SELECT starting_capital FROM portfolios WHERE id = $1`,
		portfolioID).Scan(&startingCapital)
	if err != nil {
		return decimal.Zero, fmt.Errorf("loading starting_capital: %w", err)
	}
	return startingCapital, nil
}

func (p *Performance) loadTrades(ctx context.Context, portfolioID int64) ([]tradeEvent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT executed_at, ticker, action, shares, price, fee
		FROM executed_trades
		WHERE portfolio_id = $1
		ORDER BY executed_at ASC, id ASC
	`, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("loading trades: %w", err)
	}
	defer rows.Close()
	var out []tradeEvent
	for rows.Next() {
		var e tradeEvent
		if err := rows.Scan(&e.date, &e.ticker, &e.action, &e.shares, &e.price, &e.fee); err != nil {
			return nil, fmt.Errorf("scanning trade: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating trades: %w", err)
	}
	return out, nil
}

func (p *Performance) loadCapitalEvents(ctx context.Context, portfolioID int64) ([]capEvent, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT occurred_at, amount
		FROM capital_events
		WHERE portfolio_id = $1
		ORDER BY occurred_at ASC, id ASC
	`, portfolioID)
	if err != nil {
		return nil, fmt.Errorf("loading capital events: %w", err)
	}
	defer rows.Close()
	var out []capEvent
	for rows.Next() {
		var e capEvent
		if err := rows.Scan(&e.date, &e.amount); err != nil {
			return nil, fmt.Errorf("scanning capital event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating capital events: %w", err)
	}
	return out, nil
}

// priceMap maps (ticker, date) → close, with a "last seen on or before"
// query so weekends/gaps don't introduce artificial dips. We pre-load the
// full window once instead of querying per-day per-ticker.
type priceMap struct {
	byTickerSorted map[string][]priceEntry // entries sorted by date ascending
}

type priceEntry struct {
	date  time.Time
	close decimal.Decimal
}

// LastSeen returns the latest close for `ticker` on or before `d`, and a
// found flag. If no such row exists (ticker never had data, or all data is
// after `d`), returns (Zero, false).
func (m *priceMap) LastSeen(ticker string, d time.Time) (decimal.Decimal, bool) {
	entries := m.byTickerSorted[ticker]
	if len(entries) == 0 {
		return decimal.Zero, false
	}
	// Binary search: largest index where entries[i].date <= d.
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if entries[mid].date.After(d) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	idx := lo - 1
	if idx < 0 {
		return decimal.Zero, false
	}
	return entries[idx].close, true
}

func (p *Performance) loadPriceLookup(ctx context.Context, tickers []string, from, to time.Time) (*priceMap, error) {
	if len(tickers) == 0 {
		return &priceMap{byTickerSorted: map[string][]priceEntry{}}, nil
	}
	// Pull every row from `from` going back to "ever" so LastSeen always finds
	// the prior close even when the window starts mid-week. Caller scoped to
	// reasonable horizons in practice.
	rows, err := p.pool.Query(ctx, `
		SELECT ticker, date, close FROM daily_prices
		WHERE ticker = ANY($1) AND date <= $2
		ORDER BY ticker, date ASC
	`, tickers, to)
	if err != nil {
		return nil, fmt.Errorf("loading prices: %w", err)
	}
	defer rows.Close()
	m := &priceMap{byTickerSorted: make(map[string][]priceEntry)}
	for rows.Next() {
		var (
			ticker string
			date   time.Time
			close  decimal.Decimal
		)
		if err := rows.Scan(&ticker, &date, &close); err != nil {
			return nil, fmt.Errorf("scanning price: %w", err)
		}
		m.byTickerSorted[ticker] = append(m.byTickerSorted[ticker], priceEntry{date: date, close: close})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating prices: %w", err)
	}
	// Already sorted by ORDER BY; keep the assertion for clarity.
	for _, entries := range m.byTickerSorted {
		sort.Slice(entries, func(i, j int) bool { return entries[i].date.Before(entries[j].date) })
	}
	return m, nil
}

func (p *Performance) loadSPYLookup(ctx context.Context, from, to time.Time) (*priceMap, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT ticker, date, close FROM benchmark_prices
		WHERE ticker = 'SPY' AND date <= $1
		ORDER BY date ASC
	`, to)
	if err != nil {
		return nil, fmt.Errorf("loading SPY: %w", err)
	}
	defer rows.Close()
	m := &priceMap{byTickerSorted: make(map[string][]priceEntry)}
	for rows.Next() {
		var (
			ticker string
			date   time.Time
			close  decimal.Decimal
		)
		if err := rows.Scan(&ticker, &date, &close); err != nil {
			return nil, fmt.Errorf("scanning SPY: %w", err)
		}
		m.byTickerSorted[ticker] = append(m.byTickerSorted[ticker], priceEntry{date: date, close: close})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating SPY: %w", err)
	}
	return m, nil
}

func tickersFromTrades(trades []tradeEvent) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, t := range trades {
		if _, ok := seen[t.ticker]; ok {
			continue
		}
		seen[t.ticker] = struct{}{}
		out = append(out, t.ticker)
	}
	return out
}

func applyTrade(holdings map[string]decimal.Decimal, cash *decimal.Decimal, t tradeEvent) {
	if t.action == "buy" {
		holdings[t.ticker] = holdings[t.ticker].Add(t.shares)
		*cash = cash.Sub(t.shares.Mul(t.price)).Sub(t.fee)
	} else { // sell
		holdings[t.ticker] = holdings[t.ticker].Sub(t.shares)
		*cash = cash.Add(t.shares.Mul(t.price)).Sub(t.fee)
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// indexAfter returns the smallest i in [0,n) for which pred(i) is true, or n
// if none. Standard "lower_bound" idiom; helpful for skipping past pre-window
// events when we've already applied them.
func indexAfter(n int, pred func(i int) bool) int {
	for i := 0; i < n; i++ {
		if pred(i) {
			return i
		}
	}
	return n
}
