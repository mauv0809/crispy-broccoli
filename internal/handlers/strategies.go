package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

// StrategyHandler handles strategy-related API endpoints
type StrategyHandler struct {
	repo       *strategy.Repository
	executor   *strategy.Executor
	validator  *strategy.Validator
	backtester *strategy.Backtester
}

// NewStrategyHandler creates a new strategy handler
func NewStrategyHandler(repo *strategy.Repository, executor *strategy.Executor, backtester *strategy.Backtester) *StrategyHandler {
	return &StrategyHandler{
		repo:       repo,
		executor:   executor,
		validator:  strategy.NewValidator(),
		backtester: backtester,
	}
}

// currentUserID returns the authenticated user's ID. Nil-derefs (and
// 500s via Recover) if no user is on context — that signals a write
// handler that wasn't wrapped in RequireAuth, which is a programming bug.
func currentUserID(c echo.Context) int64 {
	return auth.UserFromContext(c).ID
}

// ListStrategies returns all strategies
// @Summary List all strategies
// @Description Get a list of all saved strategies
// @Tags strategies
// @Produce json
// @Success 200 {array} strategy.Strategy
// @Router /api/strategies [get]
func (h *StrategyHandler) ListStrategies(c echo.Context) error {
	strategies, err := h.repo.List(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to list strategies",
		})
	}
	if strategies == nil {
		strategies = []strategy.Strategy{}
	}
	return c.JSON(http.StatusOK, strategies)
}

// GetStrategy returns a strategy by ID
// @Summary Get a strategy
// @Description Get a strategy by its ID
// @Tags strategies
// @Produce json
// @Param id path int true "Strategy ID"
// @Success 200 {object} strategy.Strategy
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id} [get]
func (h *StrategyHandler) GetStrategy(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "Strategy not found",
		})
	}

	return c.JSON(http.StatusOK, s)
}

// CreateStrategy creates a new strategy
// @Summary Create a strategy
// @Description Create a new stock screening strategy
// @Tags strategies
// @Accept json
// @Produce json
// @Param strategy body strategy.CreateStrategyRequest true "Strategy to create"
// @Success 201 {object} strategy.Strategy
// @Failure 400 {object} map[string]string
// @Router /api/strategies [post]
func (h *StrategyHandler) CreateStrategy(c echo.Context) error {
	var req strategy.CreateStrategyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid request body",
		})
	}

	// Validate
	if err := h.validator.ValidateStrategy(req.Name, req.Rules); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
	}

	s, err := h.repo.Create(c.Request().Context(), req, currentUserID(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to create strategy",
		})
	}

	return c.JSON(http.StatusCreated, s)
}

// UpdateStrategy updates an existing strategy
// @Summary Update a strategy
// @Description Update an existing strategy
// @Tags strategies
// @Accept json
// @Produce json
// @Param id path int true "Strategy ID"
// @Param strategy body strategy.UpdateStrategyRequest true "Strategy updates"
// @Success 200 {object} strategy.Strategy
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id} [put]
func (h *StrategyHandler) UpdateStrategy(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	var req strategy.UpdateStrategyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid request body",
		})
	}

	// Validate
	if err := h.validator.ValidateStrategy(req.Name, req.Rules); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
	}

	s, err := h.repo.Update(c.Request().Context(), id, req)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "Strategy not found",
		})
	}

	return c.JSON(http.StatusOK, s)
}

// DeleteStrategy deletes a strategy
// @Summary Delete a strategy
// @Description Delete a strategy by ID
// @Tags strategies
// @Param id path int true "Strategy ID"
// @Success 204 "No Content"
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id} [delete]
func (h *StrategyHandler) DeleteStrategy(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	if err := h.repo.Delete(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "Strategy not found",
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// RunStrategy executes a strategy and returns recommendations
// @Summary Run a strategy
// @Description Execute a strategy and get stock recommendations
// @Tags strategies
// @Produce json
// @Param id path int true "Strategy ID"
// @Success 200 {object} strategy.RunResult
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/strategies/{id}/run [post]
func (h *StrategyHandler) RunStrategy(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	// Get strategy
	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "Strategy not found",
		})
	}

	// Execute strategy
	result, err := h.executor.Execute(c.Request().Context(), s)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to execute strategy: " + err.Error(),
		})
	}

	// Save run history
	run := &strategy.StrategyRun{
		StrategyID:      s.ID,
		RunAt:           result.RunAt,
		Results:         result.Recommendations,
		ExecutionTimeMs: result.ExecutionTimeMs,
		StocksScreened:  result.StocksScreened,
		StocksMatched:   result.StocksMatched,
	}
	if err := h.repo.SaveRun(c.Request().Context(), run, currentUserID(c)); err != nil {
		// Log but don't fail the request
		c.Logger().Warnf("Failed to save strategy run: %v", err)
	}

	return c.JSON(http.StatusOK, result)
}

// GetStrategyRuns returns execution history for a strategy
// @Summary Get strategy runs
// @Description Get execution history for a strategy
// @Tags strategies
// @Produce json
// @Param id path int true "Strategy ID"
// @Param limit query int false "Maximum number of runs to return" default(10)
// @Success 200 {array} strategy.StrategyRun
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id}/runs [get]
func (h *StrategyHandler) GetStrategyRuns(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	limit := 10
	if l := c.QueryParam("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	runs, err := h.repo.GetRuns(c.Request().Context(), id, limit)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to get strategy runs",
		})
	}
	if runs == nil {
		runs = []strategy.StrategyRun{}
	}

	return c.JSON(http.StatusOK, runs)
}

// GetStrategyFields returns available fields for building strategies
// @Summary Get available fields
// @Description Get metadata about available fields for strategy filters and ranking
// @Tags strategies
// @Produce json
// @Success 200 {array} strategy.FieldMeta
// @Router /api/strategy-fields [get]
func (h *StrategyHandler) GetStrategyFields(c echo.Context) error {
	fields := strategy.GetAvailableFields()
	return c.JSON(http.StatusOK, fields)
}

// PreviewStrategy runs a strategy without saving (for testing rules)
// @Summary Preview a strategy
// @Description Test strategy rules without saving
// @Tags strategies
// @Accept json
// @Produce json
// @Param strategy body strategy.CreateStrategyRequest true "Strategy to preview"
// @Success 200 {object} strategy.RunResult
// @Failure 400 {object} map[string]string
// @Router /api/strategies/preview [post]
func (h *StrategyHandler) PreviewStrategy(c echo.Context) error {
	var req strategy.CreateStrategyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid request body",
		})
	}

	// Validate rules
	if err := h.validator.ValidateRules(req.Rules); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
	}

	// Create temporary strategy for execution
	s := &strategy.Strategy{
		ID:    0,
		Name:  req.Name,
		Rules: req.Rules,
	}

	result, err := h.executor.Execute(c.Request().Context(), s)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to execute strategy: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, result)
}

// GetStrategyStats returns statistics for a strategy
// @Summary Get strategy statistics
// @Description Get aggregate statistics for strategy runs
// @Tags strategies
// @Produce json
// @Param id path int true "Strategy ID"
// @Success 200 {object} strategy.RunStats
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id}/stats [get]
func (h *StrategyHandler) GetStrategyStats(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	stats, err := h.repo.GetRunStats(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to get strategy stats",
		})
	}

	// Check if there are actually any runs
	if stats.TotalRuns == 0 && stats.LastRunAt.Before(time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)) {
		stats.LastRunAt = time.Time{} // Zero time for no runs
	}

	return c.JSON(http.StatusOK, stats)
}

// =========== Page Handlers (HTML) ===========

// StrategiesPage renders the strategies list page
func (h *StrategyHandler) StrategiesPage(c echo.Context) error {
	strategies, err := h.repo.List(c.Request().Context())
	if err != nil {
		strategies = []strategy.Strategy{}
	}
	return Render(c, http.StatusOK, views.StrategiesList(strategies))
}

// StrategyDetailPage renders the strategy detail/execution page
func (h *StrategyHandler) StrategyDetailPage(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.Redirect(http.StatusFound, "/strategies")
	}

	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return c.Redirect(http.StatusFound, "/strategies")
	}

	return Render(c, http.StatusOK, views.StrategyDetail(*s))
}

// NewStrategyPage renders the new strategy form
func (h *StrategyHandler) NewStrategyPage(c echo.Context) error {
	fields := strategy.GetAvailableFields()
	return Render(c, http.StatusOK, views.StrategyForm(nil, fields))
}

// EditStrategyPage renders the edit strategy form
func (h *StrategyHandler) EditStrategyPage(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.Redirect(http.StatusFound, "/strategies")
	}

	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return c.Redirect(http.StatusFound, "/strategies")
	}

	fields := strategy.GetAvailableFields()
	return Render(c, http.StatusOK, views.StrategyForm(s, fields))
}

// RunStrategyHTMX executes a strategy and returns HTML results
func (h *StrategyHandler) RunStrategyHTMX(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid strategy ID"))
	}

	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return Render(c, http.StatusNotFound, views.FormError("Strategy not found"))
	}

	result, err := h.executor.Execute(c.Request().Context(), s)
	if err != nil {
		return Render(c, http.StatusInternalServerError, views.FormError("Failed to execute: "+err.Error()))
	}

	// Save run history
	run := &strategy.StrategyRun{
		StrategyID:      s.ID,
		RunAt:           result.RunAt,
		Results:         result.Recommendations,
		ExecutionTimeMs: result.ExecutionTimeMs,
		StocksScreened:  result.StocksScreened,
		StocksMatched:   result.StocksMatched,
	}
	_ = h.repo.SaveRun(c.Request().Context(), run, currentUserID(c))

	return Render(c, http.StatusOK, views.ExecutionResults(*result))
}

// GetStrategyRunsHTMX returns run history as HTML
func (h *StrategyHandler) GetStrategyRunsHTMX(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid strategy ID"))
	}

	limit := 5
	if l := c.QueryParam("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	runs, err := h.repo.GetRuns(c.Request().Context(), id, limit)
	if err != nil {
		runs = []strategy.StrategyRun{}
	}

	return Render(c, http.StatusOK, views.RunHistoryList(runs))
}

// PreviewStrategyHTMX runs a strategy preview and returns HTML
func (h *StrategyHandler) PreviewStrategyHTMX(c echo.Context) error {
	var req strategy.CreateStrategyRequest
	if err := c.Bind(&req); err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid request"))
	}

	if err := h.validator.ValidateRules(req.Rules); err != nil {
		return Render(c, http.StatusBadRequest, views.FormError(err.Error()))
	}

	s := &strategy.Strategy{
		ID:    0,
		Name:  req.Name,
		Rules: req.Rules,
	}

	result, err := h.executor.Execute(c.Request().Context(), s)
	if err != nil {
		return Render(c, http.StatusInternalServerError, views.FormError("Failed to execute: "+err.Error()))
	}

	return Render(c, http.StatusOK, views.PreviewResults(*result))
}

// DashboardStrategies returns strategies summary for dashboard
func (h *StrategyHandler) DashboardStrategies(c echo.Context) error {
	strategies, err := h.repo.List(c.Request().Context())
	if err != nil {
		strategies = []strategy.Strategy{}
	}

	// Convert to dashboard format with run counts
	dashStrategies := make([]views.DashboardStrategy, 0, len(strategies))
	for _, s := range strategies {
		stats, _ := h.repo.GetRunStats(c.Request().Context(), s.ID)
		dashStrategies = append(dashStrategies, views.DashboardStrategy{
			ID:        s.ID,
			Name:      s.Name,
			IsDefault: s.IsDefault,
			RunCount:  stats.TotalRuns,
		})
	}

	return Render(c, http.StatusOK, views.DashboardStrategies(dashStrategies))
}

// DashboardRuns returns recent runs for dashboard
func (h *StrategyHandler) DashboardRuns(c echo.Context) error {
	// Get all strategies to look up names
	strategies, _ := h.repo.List(c.Request().Context())
	strategyNames := make(map[int]string)
	for _, s := range strategies {
		strategyNames[s.ID] = s.Name
	}

	// Get recent runs across all strategies
	runs, err := h.repo.GetRecentRuns(c.Request().Context(), 10)
	if err != nil {
		runs = []strategy.StrategyRun{}
	}

	dashRuns := make([]views.DashboardRun, 0, len(runs))
	for _, r := range runs {
		dashRuns = append(dashRuns, views.DashboardRun{
			StrategyID:   r.StrategyID,
			StrategyName: strategyNames[r.StrategyID],
			RunDate:      r.RunAt.Format("Jan 2, 15:04"),
			Matches:      r.StocksMatched,
		})
	}

	return Render(c, http.StatusOK, views.DashboardRuns(dashRuns))
}

// BacktestRequest represents the request body for running a backtest
type BacktestRequest struct {
	StartDate     string  `json:"start_date"`               // Format: YYYY-MM-DD
	EndDate       string  `json:"end_date"`                 // Format: YYYY-MM-DD
	RebalanceFreq string  `json:"rebalance_freq,omitempty"` // monthly, quarterly, semi-annual, annual
	LagDays       int     `json:"lag_days,omitempty"`       // Default 60
	InitialCap    float64 `json:"initial_capital,omitempty"`
}

// RunBacktest runs a historical backtest for a strategy
// @Summary Run backtest
// @Description Run a historical backtest for a strategy against SPY benchmark
// @Tags strategies
// @Accept json
// @Produce json
// @Param id path int true "Strategy ID"
// @Param config body BacktestRequest true "Backtest configuration"
// @Success 200 {object} strategy.BacktestResult
// @Failure 400 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Router /api/strategies/{id}/backtest [post]
func (h *StrategyHandler) RunBacktest(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid strategy ID",
		})
	}

	// Get strategy
	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "Strategy not found",
		})
	}

	// Parse request body
	var req BacktestRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid request body",
		})
	}

	// Parse dates
	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid start_date format (use YYYY-MM-DD)",
		})
	}

	endDate, err := time.Parse("2006-01-02", req.EndDate)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "Invalid end_date format (use YYYY-MM-DD)",
		})
	}

	if endDate.Before(startDate) {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "end_date must be after start_date",
		})
	}

	// Build config
	config := strategy.BacktestConfig{
		StrategyID:     id,
		StartDate:      startDate,
		EndDate:        endDate,
		RebalanceFreq:  req.RebalanceFreq,
		LagDays:        req.LagDays,
		InitialCapital: req.InitialCap,
	}

	// Run backtest
	result, err := h.backtester.Run(c.Request().Context(), s, config)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Backtest failed: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, result)
}

// RunBacktestHTMX runs a backtest and returns HTML results
func (h *StrategyHandler) RunBacktestHTMX(c echo.Context) error {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid strategy ID"))
	}

	s, err := h.repo.GetByID(c.Request().Context(), id)
	if err != nil {
		return Render(c, http.StatusNotFound, views.FormError("Strategy not found"))
	}

	// Parse form values
	startDateStr := c.FormValue("start_date")
	endDateStr := c.FormValue("end_date")
	rebalanceFreq := c.FormValue("rebalance_freq")
	initialCapitalStr := c.FormValue("initial_capital")

	if startDateStr == "" || endDateStr == "" {
		return Render(c, http.StatusBadRequest, views.FormError("Start date and end date are required"))
	}

	startDate, err := time.Parse("2006-01-02", startDateStr)
	if err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid start date format"))
	}

	endDate, err := time.Parse("2006-01-02", endDateStr)
	if err != nil {
		return Render(c, http.StatusBadRequest, views.FormError("Invalid end date format"))
	}

	if rebalanceFreq == "" {
		rebalanceFreq = "semi-annual"
	}

	initialCapital := 10000.0
	if initialCapitalStr != "" {
		if parsed, err := strconv.ParseFloat(initialCapitalStr, 64); err == nil && parsed > 0 {
			initialCapital = parsed
		}
	}

	config := strategy.BacktestConfig{
		StrategyID:     id,
		StartDate:      startDate,
		EndDate:        endDate,
		RebalanceFreq:  rebalanceFreq,
		LagDays:        60,
		InitialCapital: initialCapital,
	}

	result, err := h.backtester.Run(c.Request().Context(), s, config)
	if err != nil {
		return Render(c, http.StatusInternalServerError, views.FormError("Backtest failed: "+err.Error()))
	}

	return Render(c, http.StatusOK, views.BacktestResults(*result))
}
