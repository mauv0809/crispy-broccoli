package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/dbutil"
	"github.com/mauv0809/crispy-broccoli/internal/observability"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/scheduler"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

// PortfoliosHandler bundles the dependencies for the portfolios UI. The
// PickGenerator and Mailer interfaces are reused from the scheduler package
// so production wires the same concrete types in both places.
type PortfoliosHandler struct {
	pool          *pgxpool.Pool
	service       *portfolio.Service
	portfolios    *portfolio.Repository
	holdings      *portfolio.Holdings
	performance   *portfolio.Performance
	proposals     *proposal.Repository
	strategies    *strategy.Repository
	versions      *strategy.VersionsRepository
	pickGenerator scheduler.PickGenerator
	mailer        scheduler.Mailer
}

type PortfoliosDeps struct {
	Pool          *pgxpool.Pool
	Service       *portfolio.Service
	Portfolios    *portfolio.Repository
	Holdings      *portfolio.Holdings
	Performance   *portfolio.Performance
	Proposals     *proposal.Repository
	Strategies    *strategy.Repository
	Versions      *strategy.VersionsRepository
	PickGenerator scheduler.PickGenerator
	Mailer        scheduler.Mailer
}

func NewPortfoliosHandler(d PortfoliosDeps) *PortfoliosHandler {
	return &PortfoliosHandler{
		pool: d.Pool, service: d.Service, portfolios: d.Portfolios, holdings: d.Holdings,
		performance: d.Performance,
		proposals:   d.Proposals, strategies: d.Strategies, versions: d.Versions,
		pickGenerator: d.PickGenerator, mailer: d.Mailer,
	}
}

// List renders the user's non-archived portfolios.
func (h *PortfoliosHandler) List(c echo.Context) error {
	ctx := c.Request().Context()
	uid := currentUserID(c)
	ports, err := h.portfolios.ListByUser(ctx, uid)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	items := make([]views.PortfolioListItem, 0, len(ports))
	for _, p := range ports {
		// Strategy name (best-effort; UI tolerates missing).
		var strategyName string
		if strat, err := h.strategies.GetByID(ctx, p.StrategyID); err == nil {
			strategyName = strat.Name
		}

		// Pending-proposal flag.
		hasPending := false
		if _, err := h.proposals.GetPending(ctx, h.pool, p.ID); err == nil {
			hasPending = true
		}

		snap, err := h.performance.Current(ctx, p.ID)
		if err != nil {
			// Log but render the card with zeros — performance is non-critical for the list view.
			observability.CaptureHandlerError(c, err)
			snap = &portfolio.Snapshot{
				MarketValue:  p.StartingCapital,
				ReturnAmount: decimal.Zero,
				ReturnPct:    decimal.Zero,
			}
		}

		items = append(items, views.PortfolioListItem{
			Portfolio:    p,
			StrategyName: strategyName,
			CurrentValue: snap.MarketValue,
			ReturnAmount: snap.ReturnAmount,
			ReturnPct:    snap.ReturnPct,
			HasPending:   hasPending,
		})
	}
	return Render(c, http.StatusOK, views.PortfoliosList(items))
}

// NewForm renders the create form with verified strategies for the picker.
func (h *PortfoliosHandler) NewForm(c echo.Context) error {
	ctx := c.Request().Context()
	verified, err := h.strategies.ListVerified(ctx)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	choices := make([]views.StrategyChoice, len(verified))
	for i, s := range verified {
		choices[i] = views.StrategyChoice{
			ID: s.ID, Name: s.Name, DefaultCadence: s.DefaultCadence,
		}
	}
	return Render(c, http.StatusOK, views.PortfolioForm(choices, ""))
}

// Create validates the form, creates the portfolio via the service, then
// synchronously generates the first proposal so the user lands on the
// review page with picks already computed. Email send is best-effort
// (failures are logged but don't block the redirect).
func (h *PortfoliosHandler) Create(c echo.Context) error {
	ctx := c.Request().Context()
	uid := currentUserID(c)

	startingCapital, err := decimal.NewFromString(c.FormValue("starting_capital"))
	if err != nil || startingCapital.LessThanOrEqual(decimal.Zero) {
		return h.renderFormError(c, "Starting capital must be a positive number")
	}
	strategyID, err := strconv.ParseInt(c.FormValue("strategy_id"), 10, 64)
	if err != nil {
		return h.renderFormError(c, "Invalid strategy")
	}
	name := c.FormValue("name")
	if name == "" {
		return h.renderFormError(c, "Name is required")
	}

	var cadenceOverride *strategy.Cadence
	if v := c.FormValue("cadence"); v != "" {
		cv := strategy.Cadence(v)
		cadenceOverride = &cv
	}

	port, err := h.service.CreatePortfolio(ctx, portfolio.CreatePortfolioInput{
		UserID:          uid,
		Name:            name,
		StartingCapital: startingCapital,
		StrategyID:      strategyID,
		CadenceOverride: cadenceOverride,
	})
	if err != nil {
		switch {
		case errors.Is(err, portfolio.ErrStrategyNotVerified):
			return h.renderFormError(c, "The selected strategy must be verified before it can be used")
		case errors.Is(err, portfolio.ErrCadenceMissing):
			return h.renderFormError(c, "Pick a cadence — the strategy doesn't define a default")
		}
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	// Synchronously generate first proposal. If this fails, the portfolio
	// still exists; the user can re-trigger from the detail page (future).
	prID, err := h.generateFirstProposal(ctx, port)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return c.Redirect(http.StatusSeeOther,
			fmt.Sprintf("/portfolios/%d?generation_error=1", port.ID))
	}

	// Email is best-effort. Failure is logged via the scheduler retry path.
	if err := h.mailer.SendProposalReady(ctx, prID); err == nil {
		_ = h.proposals.SetNotificationSent(ctx, prID, time.Now().UTC())
	}

	return c.Redirect(http.StatusSeeOther,
		fmt.Sprintf("/portfolios/%d/proposals/%d", port.ID, prID))
}

// generateFirstProposal mirrors the scheduler's per-portfolio path but bypasses
// the "find due portfolios" step — we know exactly which portfolio just got
// created. Runs in one transaction so partial state can't leak.
func (h *PortfoliosHandler) generateFirstProposal(ctx context.Context, p *portfolio.Portfolio) (int64, error) {
	var prID int64
	err := dbutil.RunInTx(ctx, h.pool, func(tx dbutil.DBTX) error {
		ver, err := h.versions.Get(ctx, p.StrategyVersionID)
		if err != nil {
			return fmt.Errorf("loading strategy version: %w", err)
		}
		picks, err := h.pickGenerator.GeneratePicks(ctx, proposal.GenerateInput{
			PortfolioID:   p.ID,
			Rules:         ver.Rules,
			MarketValue:   p.StartingCapital,
			CapitalChange: decimal.Zero,
			StrategyLimit: 0,
		})
		if err != nil {
			return fmt.Errorf("generating picks: %w", err)
		}
		pr, err := h.proposals.Insert(ctx, tx, proposal.InsertInput{
			PortfolioID:           p.ID,
			StrategyVersionID:     p.StrategyVersionID,
			MarketValueAtProposal: p.StartingCapital,
			CapitalChange:         decimal.Zero,
			DeployAmount:          p.StartingCapital,
			Picks:                 picks,
		})
		if err != nil {
			return fmt.Errorf("inserting proposal: %w", err)
		}
		prID = pr.ID
		return nil
	})
	return prID, err
}

// renderFormError re-renders the create form with an inline error banner.
// Re-fetches the strategy choices so the form state is consistent.
func (h *PortfoliosHandler) renderFormError(c echo.Context, msg string) error {
	ctx := c.Request().Context()
	verified, err := h.strategies.ListVerified(ctx)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	choices := make([]views.StrategyChoice, len(verified))
	for i, s := range verified {
		choices[i] = views.StrategyChoice{
			ID: s.ID, Name: s.Name, DefaultCadence: s.DefaultCadence,
		}
	}
	return Render(c, http.StatusBadRequest, views.PortfolioForm(choices, msg))
}

// Detail renders the portfolio page with holdings, history, and (if any)
// pending proposal CTA. Auth-checked: 404 if the user doesn't own this
// portfolio and isn't admin (don't leak existence).
func (h *PortfoliosHandler) Detail(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	ctx := c.Request().Context()

	p, err := h.portfolios.GetByID(ctx, id)
	if errors.Is(err, portfolio.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	strat, _ := h.strategies.GetByID(ctx, p.StrategyID)
	var ver *strategy.Version
	if v, err := h.versions.Get(ctx, p.StrategyVersionID); err == nil {
		ver = v
	}
	pending, _ := h.proposals.GetPending(ctx, h.pool, p.ID)

	hlds, err := h.holdings.ListByPortfolio(ctx, p.ID)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	rows := make([]views.HoldingRow, 0, len(hlds))
	for _, hld := range hlds {
		rows = append(rows, views.HoldingRow{Holding: hld})
	}

	snap, err := h.performance.Current(ctx, p.ID)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}

	d := views.PortfolioDetailData{
		Portfolio:       *p,
		StrategyName:    safeStrategyName(strat),
		StrategyVersion: safeVersionNumber(ver),
		Holdings:        rows,
		History:         nil,
		PendingProposal: pending,
		CurrentValue:    snap.MarketValue,
		NetInvested:     snap.NetInvested,
		ReturnAmount:    snap.ReturnAmount,
		ReturnPct:       snap.ReturnPct,
	}
	return Render(c, http.StatusOK, views.PortfolioDetail(d))
}

// PerformanceJSON returns the portfolio's daily value time series + SPY
// normalised series as JSON, used by the Chart.js chart on the detail page.
// The window is "since portfolio creation" through today.
func (h *PortfoliosHandler) PerformanceJSON(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	ctx := c.Request().Context()

	p, err := h.portfolios.GetByID(ctx, id)
	if errors.Is(err, portfolio.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound)
	}

	to := time.Now().UTC().Truncate(24 * time.Hour)
	from := p.CreatedAt.UTC().Truncate(24 * time.Hour)
	series, err := h.performance.TimeSeries(ctx, id, from, to)
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.JSON(http.StatusOK, series)
}

func (h *PortfoliosHandler) Pause(c echo.Context) error {
	return h.setStatus(c, portfolio.StatusPaused)
}
func (h *PortfoliosHandler) Resume(c echo.Context) error {
	return h.setStatus(c, portfolio.StatusActive)
}
func (h *PortfoliosHandler) Archive(c echo.Context) error {
	return h.setStatus(c, portfolio.StatusArchived)
}

func (h *PortfoliosHandler) setStatus(c echo.Context, status portfolio.Status) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest)
	}
	ctx := c.Request().Context()
	p, err := h.portfolios.GetByID(ctx, id)
	if errors.Is(err, portfolio.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	if !canAccessPortfolio(c, p) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := h.service.SetStatus(ctx, id, status); err != nil {
		observability.CaptureHandlerError(c, err)
		return echo.NewHTTPError(http.StatusInternalServerError)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/portfolios/%d", id))
}

// canAccessPortfolio: owner or admin only. Returns false → handlers should
// 404 (not 403) so we don't leak portfolio existence.
func canAccessPortfolio(c echo.Context, p *portfolio.Portfolio) bool {
	user := auth.UserFromContext(c)
	if user == nil {
		return false
	}
	return user.ID == p.UserID || user.IsAdmin
}

func safeStrategyName(s *strategy.Strategy) string {
	if s == nil {
		return ""
	}
	return s.Name
}
func safeVersionNumber(v *strategy.Version) int {
	if v == nil {
		return 0
	}
	return v.VersionNumber
}
