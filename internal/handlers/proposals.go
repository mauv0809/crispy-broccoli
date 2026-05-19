package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/observability"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/scheduler"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

// ProposalsHandler bundles the deps for the proposal review/accept/skip flow.
// PickGenerator is reused from the scheduler package (same interface) so the
// recompute path uses the same generator as the scheduler's tick.
type ProposalsHandler struct {
	pool       *pgxpool.Pool
	portfolios *portfolio.Repository
	proposals  *proposal.Repository
	strategies *strategy.Repository
	versions   *strategy.VersionsRepository
	acceptor   *proposal.Acceptor
	pickGen    scheduler.PickGenerator
}

type ProposalsDeps struct {
	Pool       *pgxpool.Pool
	Portfolios *portfolio.Repository
	Proposals  *proposal.Repository
	Strategies *strategy.Repository
	Versions   *strategy.VersionsRepository
	Acceptor   *proposal.Acceptor
	PickGen    scheduler.PickGenerator
}

func NewProposalsHandler(d ProposalsDeps) *ProposalsHandler {
	return &ProposalsHandler{
		pool: d.Pool, portfolios: d.Portfolios, proposals: d.Proposals,
		strategies: d.Strategies, versions: d.Versions,
		acceptor: d.Acceptor, pickGen: d.PickGen,
	}
}

// Detail renders the review page. The picks table is rendered inline as part
// of the page; the same fragment is what Recompute returns for HTMX swaps.
func (h *ProposalsHandler) Detail(c echo.Context) error {
	portfolioID, proposalID, err := parseProposalRoute(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	port, pr, err := h.loadAndAuthorize(c, portfolioID, proposalID)
	if err != nil {
		return err
	}

	var stratName string
	if s, err := h.strategies.GetByID(ctx, port.StrategyID); err == nil {
		stratName = s.Name
	}

	return Render(c, http.StatusOK, views.ProposalDetail(views.ProposalDetailData{
		Portfolio:    *port,
		Proposal:     *pr,
		StrategyName: stratName,
	}))
}

// Recompute is the HTMX endpoint hit when the user changes capital_change.
// It re-runs the generator with the new amount, mutates the pending proposal
// in place (UpdatePending), and returns the picks-table fragment for swap.
//
// Returning the same fragment id (`#picks-table`) means HTMX outerHTML-swaps
// it without disturbing the surrounding form.
func (h *ProposalsHandler) Recompute(c echo.Context) error {
	portfolioID, proposalID, err := parseProposalRoute(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	port, pr, err := h.loadAndAuthorize(c, portfolioID, proposalID)
	if err != nil {
		return err
	}
	if pr.Status != proposal.StatusPending {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("proposal not pending (status=%s)", pr.Status))
	}

	capChange, err := decimal.NewFromString(c.FormValue("capital_change"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid capital_change")
	}
	if capChange.IsNegative() && capChange.Abs().GreaterThan(pr.MarketValueAtProposal) {
		return echo.NewHTTPError(http.StatusBadRequest, "withdrawal exceeds market value")
	}

	ver, err := h.versions.Get(ctx, port.StrategyVersionID)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	picks, err := h.pickGen.GeneratePicks(ctx, proposal.GenerateInput{
		PortfolioID:   port.ID,
		Rules:         ver.Rules,
		MarketValue:   pr.MarketValueAtProposal,
		CapitalChange: capChange,
		StrategyLimit: 0,
	})
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	deploy := pr.MarketValueAtProposal.Add(capChange)
	if err := h.proposals.UpdatePending(ctx, h.pool, pr.ID, proposal.UpdatePendingInput{
		CapitalChange: capChange,
		DeployAmount:  deploy,
		Picks:         picks,
	}); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	return Render(c, http.StatusOK, views.ProposalPicksTable(views.PicksTableData{
		ProposalID:   pr.ID,
		PortfolioID:  port.ID,
		Picks:        picks,
		DeployAmount: deploy,
	}))
}

// Accept parses per-row decisions (paired-array form encoding from the picks
// table), calls the acceptor, redirects to portfolio detail on success.
func (h *ProposalsHandler) Accept(c echo.Context) error {
	portfolioID, proposalID, err := parseProposalRoute(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	_, _, err = h.loadAndAuthorize(c, portfolioID, proposalID)
	if err != nil {
		return err
	}

	form, err := c.FormParams()
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid form encoding")
	}
	rows, perr := parseRowDecisions(form)
	if perr != nil {
		return echo.NewHTTPError(http.StatusBadRequest, perr.Error())
	}

	if _, err := h.acceptor.Accept(ctx, proposalID, proposal.AcceptInput{
		Now:  time.Now().UTC(),
		Rows: rows,
	}); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d", portfolioID))
}

// Skip discards the proposal without trades, but still advances the cadence.
func (h *ProposalsHandler) Skip(c echo.Context) error {
	portfolioID, proposalID, err := parseProposalRoute(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	_, _, err = h.loadAndAuthorize(c, portfolioID, proposalID)
	if err != nil {
		return err
	}

	if err := h.acceptor.Skip(ctx, proposalID, time.Now().UTC()); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	// Skip can be triggered via HTMX (hx-target="body") or a regular form
	// submit. For HTMX requests, return an HX-Redirect header. Otherwise
	// 303 redirect.
	if c.Request().Header.Get("HX-Request") == "true" {
		c.Response().Header().Set("HX-Redirect", fmt.Sprintf("/portfolios/%d", portfolioID))
		return c.NoContent(http.StatusOK)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d", portfolioID))
}

// parseProposalRoute extracts and validates the (:id, :pid) path params.
func parseProposalRoute(c echo.Context) (portfolioID, proposalID int64, err error) {
	pid, err1 := strconv.ParseInt(c.Param("id"), 10, 64)
	prID, err2 := strconv.ParseInt(c.Param("pid"), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, echo.NewHTTPError(http.StatusBadRequest)
	}
	return pid, prID, nil
}

// loadAndAuthorize loads the portfolio + proposal, verifies the proposal
// belongs to the portfolio, and checks ownership/admin. Returns echo.HTTPError
// (404) on any mismatch or auth failure to keep responses uniform.
func (h *ProposalsHandler) loadAndAuthorize(c echo.Context, portfolioID, proposalID int64) (*portfolio.Portfolio, *proposal.Proposal, error) {
	ctx := c.Request().Context()
	port, err := h.portfolios.GetByID(ctx, portfolioID)
	if errors.Is(err, portfolio.ErrNotFound) {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return nil, nil, echo.NewHTTPError(http.StatusInternalServerError)
	}
	if !canAccessPortfolio(c, port) {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound)
	}

	pr, err := h.proposals.Get(ctx, proposalID)
	if errors.Is(err, proposal.ErrNotFound) {
		return nil, nil, echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return nil, nil, echo.NewHTTPError(http.StatusInternalServerError)
	}
	if pr.PortfolioID != port.ID {
		// Proposal belongs to a different portfolio — 404 to avoid leaking.
		return nil, nil, echo.NewHTTPError(http.StatusNotFound)
	}
	return port, pr, nil
}

// parseRowDecisions reads paired-array form fields from the picks table.
// The picks table renders inputs without indices: each pick contributes one
// `<input name="ticker">`, one `<input name="actual_shares">`, etc. Echo's
// FormParams returns these as []string keyed by name. We zip them by index.
//
// "skip" is a checkbox that only appears in the form values when checked.
// To pair skip values back to row indices, the picks table renders a hidden
// "row_idx" input on each row alongside the checkbox; the checkbox uses
// the row's ticker as its value when checked. We map ticker → skip rather
// than relying on positional alignment for skip, since checkboxes don't
// emit a value when unchecked.
//
// Form structure:
//
//	ticker[i], actual_shares[i], actual_price[i], fee[i] are positional
//	skip is checkbox; checked-only emits value=ticker so we can match by ticker.
func parseRowDecisions(form url.Values) ([]proposal.RowDecision, error) {
	tickers := form["ticker"]
	shares := form["actual_shares"]
	prices := form["actual_price"]
	fees := form["fee"]
	skips := form["skip"]

	if len(tickers) == 0 {
		return nil, fmt.Errorf("no rows submitted")
	}
	if len(shares) != len(tickers) || len(prices) != len(tickers) || len(fees) != len(tickers) {
		return nil, fmt.Errorf("mismatched row arrays: tickers=%d shares=%d prices=%d fees=%d",
			len(tickers), len(shares), len(prices), len(fees))
	}

	skipped := make(map[string]bool, len(skips))
	for _, s := range skips {
		// skip checkbox value is the ticker (set by the picks_table view via
		// value={p.Ticker}). The form submits it only when checked, so we
		// build a set keyed by ticker.
		skipped[s] = true
	}

	rows := make([]proposal.RowDecision, 0, len(tickers))
	for i, t := range tickers {
		row := proposal.RowDecision{Ticker: t}
		if skipped[t] {
			row.Skip = true
		} else {
			if shares[i] != "" {
				v, err := decimal.NewFromString(shares[i])
				if err != nil {
					return nil, fmt.Errorf("row %d: invalid actual_shares %q", i, shares[i])
				}
				row.ActualShares = v
			}
			if prices[i] != "" {
				v, err := decimal.NewFromString(prices[i])
				if err != nil {
					return nil, fmt.Errorf("row %d: invalid actual_price %q", i, prices[i])
				}
				row.ActualPrice = v
			}
			if fees[i] != "" {
				v, err := decimal.NewFromString(fees[i])
				if err != nil {
					return nil, fmt.Errorf("row %d: invalid fee %q", i, fees[i])
				}
				row.Fee = v
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
