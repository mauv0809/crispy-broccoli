package handlers_test

// HTTP-layer smoke tests. The other handler tests in this package call
// handler methods directly with manually-built Echo contexts, which means
// they bypass middleware entirely. That's fine for handler logic but
// hides bugs at the seam between middleware config and view rendering —
// most notably CSRF (the token field name has to match the middleware's
// TokenLookup) and authentication redirects.
//
// These tests wire the *real* middleware stack (CSRF + RequireAuth using
// a stub session) into an *echo.Echo and exercise it through
// httptest.NewServer. One test per write endpoint, plus a regression test
// for the CSRF field-name bug we shipped earlier.

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/shopspring/decimal"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/handlers"
	"github.com/mauv0809/crispy-broccoli/internal/portfolio"
	"github.com/mauv0809/crispy-broccoli/internal/proposal"
	"github.com/mauv0809/crispy-broccoli/internal/strategy"
	"github.com/mauv0809/crispy-broccoli/internal/testutil"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// stubSession satisfies auth.Session — returns a fixed user id regardless
// of the actual SCS session state. Bypasses cookies entirely so tests
// don't need to drive the magic-link or Google OAuth flow.
type stubSession struct{ id int64 }

func (s stubSession) UserID(_ echo.Context) int64 { return s.id }

// stubLoader satisfies auth.UserLoader — returns a pre-built user.
type stubLoader struct{ user *users.User }

func (s stubLoader) LoadUser(_ context.Context, _ int64) (*users.User, error) {
	return s.user, nil
}

// smokeApp wires a fully middleware'd *echo.Echo identical to production
// but with stubbed session + loader so we don't need SCS or magic-link
// machinery. Returns the echo, the test server, and the pre-authenticated
// HTTP client (with cookie jar so CSRF cookies persist across requests).
type smokeApp struct {
	server       *httptest.Server
	client       *http.Client
	user         *users.User
	portfoliosH  *handlers.PortfoliosHandler
	proposalsH   *handlers.ProposalsHandler
	strategyH    *handlers.StrategyHandler
	strategyRepo *strategy.Repository
	pool         interface{ /* opaque */ } // not used directly; stored for re-use in subtests
}

func newSmokeApp(t *testing.T) *smokeApp {
	t.Helper()
	pool := testutil.OpenTestDB(t)
	uid := systemUserID(t, pool)

	// Real handlers, real repos.
	sRepo := strategy.NewRepository(pool)
	pRepo := portfolio.NewRepository(pool)
	holds := portfolio.NewHoldings(pool)
	prRepo := proposal.NewRepository(pool)
	versionsRepo := strategy.NewVersionsRepository(pool)
	svc := portfolio.NewService(pRepo, sRepo)
	performance := portfolio.NewPerformance(pool)

	portH := handlers.NewPortfoliosHandler(handlers.PortfoliosDeps{
		Pool: pool, Service: svc, Portfolios: pRepo, Holdings: holds,
		Proposals: prRepo, Strategies: sRepo, Versions: versionsRepo,
		PickGenerator: stubPickGenerator{}, Mailer: stubMailer{},
		Performance: performance,
	})
	acceptor := proposal.NewAcceptor(pool, prRepo, pRepo, holds)
	prH := handlers.NewProposalsHandler(handlers.ProposalsDeps{
		Pool: pool, Portfolios: pRepo, Proposals: prRepo,
		Strategies: sRepo, Versions: versionsRepo,
		Acceptor: acceptor, PickGen: stubPickGenerator{},
	})
	executor := strategy.NewExecutor(pool)
	stratH := handlers.NewStrategyHandler(sRepo, executor, nil)
	stratH.SetVersionsRepository(versionsRepo)

	// Test user. Lives in DB so the strategies seeded with `created_by = uid`
	// pass the FK check. We don't seed a fresh user — reuse the system user.
	user := &users.User{ID: uid, Email: "system@deepvalue.local", IsActive: true, IsAdmin: true}

	e := echo.New()
	e.Use(middleware.BodyLimit("1M"))
	e.Use(middleware.Recover())
	e.Use(middleware.CSRFWithConfig(middleware.CSRFConfig{
		// Production config — same TokenLookup, same cookie name.
		TokenLookup:    "header:X-CSRF-Token,form:_csrf",
		CookieName:     "_csrf",
		CookiePath:     "/",
		CookieHTTPOnly: false,
		CookieSameSite: http.SameSiteLaxMode,
		Skipper: func(c echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/health" || strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/auth/")
		},
	}))
	authMW := auth.RequireAuth(stubSession{id: uid}, stubLoader{user: user})

	// Routes — only the ones we want to smoke-test. Mirrors cmd/app/main.go.
	e.GET("/portfolios", portH.List, authMW)
	e.GET("/portfolios/new", portH.NewForm, authMW)
	e.POST("/portfolios", portH.Create, authMW)
	e.GET("/portfolios/:id", portH.Detail, authMW)
	e.POST("/portfolios/:id/pause", portH.Pause, authMW)
	e.POST("/portfolios/:id/resume", portH.Resume, authMW)
	e.POST("/portfolios/:id/archive", portH.Archive, authMW)
	e.GET("/portfolios/:id/proposals/:pid", prH.Detail, authMW)
	e.POST("/portfolios/:id/proposals/:pid/recompute", prH.Recompute, authMW)
	e.POST("/portfolios/:id/proposals/:pid/accept", prH.Accept, authMW)
	e.POST("/portfolios/:id/proposals/:pid/skip", prH.Skip, authMW)
	e.POST("/strategies/:id/verify", stratH.Verify, authMW)
	e.POST("/strategies/:id/archive", stratH.Archive, authMW)

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		// Don't auto-follow redirects — we want to assert on 303s.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &smokeApp{
		server: srv, client: client, user: user,
		portfoliosH: portH, proposalsH: prH, strategyH: stratH,
		strategyRepo: sRepo, pool: pool,
	}
}

// csrfInputRe finds <input ... name="_csrf" ... value="..."> regardless of
// attribute order. Echo's CSRF cookie value is the same as what we expect
// in the form, so we could also pull it out of the cookie jar — extracting
// from the body is what asserts the view rendered the right field name.
var csrfInputRe = regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*value="([^"]+)"[^>]*>`)

// extractCSRFToken pulls the _csrf hidden input value out of an HTML body
// AND verifies the field name is exactly `_csrf` (the bug was `csrf`).
// Returns the token. Calls t.Fatalf on any structural mismatch.
func extractCSRFToken(t *testing.T, body string) string {
	t.Helper()
	matches := csrfInputRe.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		if m[1] == "_csrf" {
			return m[2]
		}
	}
	// If we got here, every hidden input we found was misnamed. Surface
	// the names so failures point straight at the bug.
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	t.Fatalf("no <input name=\"_csrf\"> found in form; saw inputs named %v", names)
	return ""
}

// TestHTTP_PortfolioForm_RendersCSRFFieldWithCorrectName is the regression
// test for the bug we shipped (the form had name="csrf"; middleware looks
// for "_csrf"; every POST 400'd). Asserts the rendered form contains a
// hidden input named exactly "_csrf".
func TestHTTP_PortfolioForm_RendersCSRFFieldWithCorrectName(t *testing.T) {
	app := newSmokeApp(t)
	resp, err := app.client.Get(app.server.URL + "/portfolios/new")
	if err != nil {
		t.Fatalf("GET /portfolios/new: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = extractCSRFToken(t, string(body)) // fatals if name="_csrf" is missing
}

// TestHTTP_PortfolioCreate_HappyPath exercises the full create flow through
// real CSRF middleware: GET form, extract token, POST with the token, expect
// a 303 redirect to the new proposal review page.
func TestHTTP_PortfolioCreate_HappyPath(t *testing.T) {
	app := newSmokeApp(t)

	// Seed a verified strategy with a default cadence so the form has
	// something to pick.
	cadence := strategy.CadenceQuarterly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := app.strategyRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules, DefaultCadence: &cadence}, app.user.ID)
	if err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	if err := app.strategyRepo.Verify(context.Background(), int64(s.ID)); err != nil {
		t.Fatalf("verify strategy: %v", err)
	}

	// 1. GET /portfolios/new to receive a CSRF token + cookie.
	getResp, err := app.client.Get(app.server.URL + "/portfolios/new")
	if err != nil {
		t.Fatalf("GET form: %v", err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	token := extractCSRFToken(t, string(body))

	// 2. POST /portfolios with the token + form fields. Cookie jar carries
	// the _csrf cookie automatically.
	form := url.Values{}
	form.Set("_csrf", token)
	form.Set("name", "Smoke")
	form.Set("starting_capital", "10000")
	form.Set("strategy_id", strconv.Itoa(s.ID))
	form.Set("cadence", "quarterly")

	postResp, err := app.client.PostForm(app.server.URL+"/portfolios", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusSeeOther {
		buf, _ := io.ReadAll(postResp.Body)
		t.Fatalf("status = %d, want 303; body=%s", postResp.StatusCode, string(buf))
	}
	loc := postResp.Header.Get("Location")
	if !strings.Contains(loc, "/portfolios/") || !strings.Contains(loc, "/proposals/") {
		t.Errorf("Location = %q, want /portfolios/:id/proposals/:pid", loc)
	}
}

// TestHTTP_PortfolioCreate_MissingCSRFReturns400 confirms the middleware
// is actually engaged. Without this, a future regression that disabled
// CSRF entirely would silently pass the happy-path test above.
func TestHTTP_PortfolioCreate_MissingCSRFReturns400(t *testing.T) {
	app := newSmokeApp(t)
	form := url.Values{}
	form.Set("name", "NoToken")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", "1")

	resp, err := app.client.PostForm(app.server.URL+"/portfolios", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (CSRF rejected)", resp.StatusCode)
	}
}

// TestHTTP_StrategyVerify_HappyPath smoke-tests the strategy lifecycle
// extension via a real POST. The X-CSRF-Token header path is exercised
// here (rather than form encoding) because it's how HTMX requests would
// hit this endpoint in practice.
func TestHTTP_StrategyVerify_HappyPath(t *testing.T) {
	app := newSmokeApp(t)

	// Seed a draft strategy.
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, err := app.strategyRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-draft", Rules: rules}, app.user.ID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Prime the CSRF cookie via a GET.
	primeResp, _ := app.client.Get(app.server.URL + "/portfolios")
	primeResp.Body.Close()
	var token string
	for _, c := range app.client.Jar.Cookies(mustParseURL(t, app.server.URL)) {
		if c.Name == "_csrf" {
			token = c.Value
			break
		}
	}
	if token == "" {
		t.Fatal("CSRF cookie not set after GET")
	}

	req, _ := http.NewRequest(http.MethodPost,
		app.server.URL+"/strategies/"+strconv.Itoa(s.ID)+"/verify", nil)
	req.Header.Set("X-CSRF-Token", token)

	resp, err := app.client.Do(req)
	if err != nil {
		t.Fatalf("POST verify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		buf, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 303; body=%s", resp.StatusCode, string(buf))
	}

	// Confirm the strategy is now verified.
	got, _ := app.strategyRepo.GetByID(context.Background(), s.ID)
	if got.Status != strategy.StatusVerified {
		t.Errorf("status = %s, want verified", got.Status)
	}

	// Negative: same request without the header should be rejected.
	resp2, err := app.client.Post(
		app.server.URL+"/strategies/"+strconv.Itoa(s.ID)+"/verify",
		"", nil)
	if err != nil {
		t.Fatalf("POST without token: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status without CSRF = %d, want 400", resp2.StatusCode)
	}
}

// TestHTTP_PortfolioPause_FullFlow smoke-tests the lifecycle action via the
// detail page → button submit path. Asserts the form on the detail page
// uses _csrf as the field name, and the POST goes through.
func TestHTTP_PortfolioPause_FullFlow(t *testing.T) {
	app := newSmokeApp(t)

	// Seed a portfolio via the create flow (also exercises the form once
	// more; if the create test passes this should reliably succeed).
	cadence := strategy.CadenceQuarterly
	rules := strategy.Rules{Filters: []strategy.Filter{}, Ranking: []strategy.Ranking{}, Limit: 6, Dimension: "MRQ"}
	s, _ := app.strategyRepo.Create(context.Background(),
		strategy.CreateStrategyRequest{Name: t.Name() + "-strat", Rules: rules, DefaultCadence: &cadence}, app.user.ID)
	_ = app.strategyRepo.Verify(context.Background(), int64(s.ID))

	getForm, _ := app.client.Get(app.server.URL + "/portfolios/new")
	formBody, _ := io.ReadAll(getForm.Body)
	getForm.Body.Close()
	token := extractCSRFToken(t, string(formBody))

	form := url.Values{}
	form.Set("_csrf", token)
	form.Set("name", "Pause-test")
	form.Set("starting_capital", "1000")
	form.Set("strategy_id", strconv.Itoa(s.ID))

	createResp, _ := app.client.PostForm(app.server.URL+"/portfolios", form)
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", createResp.StatusCode)
	}
	loc := createResp.Header.Get("Location")
	parts := strings.Split(loc, "/")
	if len(parts) < 3 {
		t.Fatalf("unexpected Location: %q", loc)
	}
	pidStr := parts[2]

	// Now hit /portfolios/:id to render the detail page (which has the
	// pause/resume/archive form). Confirm the form's _csrf field renders
	// correctly.
	detailResp, _ := app.client.Get(app.server.URL + "/portfolios/" + pidStr)
	detailBody, _ := io.ReadAll(detailResp.Body)
	detailResp.Body.Close()
	detailToken := extractCSRFToken(t, string(detailBody))

	// POST /portfolios/:id/pause with the token.
	pauseForm := url.Values{}
	pauseForm.Set("_csrf", detailToken)
	pauseResp, _ := app.client.PostForm(
		app.server.URL+"/portfolios/"+pidStr+"/pause", pauseForm)
	defer pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusSeeOther {
		buf, _ := io.ReadAll(pauseResp.Body)
		t.Errorf("pause status = %d, want 303; body=%s", pauseResp.StatusCode, string(buf))
	}
}

// mustParseURL is a tiny helper for cookie-jar lookups — the jar takes a
// *url.URL, not a string.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u
}

// Sanity: make sure the decimal package is referenced so the import block
// stays honest if a test refactor drops a usage. Cheaper than annotating
// each test where decimal might appear.
var _ = decimal.Zero
