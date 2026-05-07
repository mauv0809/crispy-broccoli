package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/auth"
	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// fakeLoader implements auth.UserLoader for in-process middleware tests.
type fakeLoader struct {
	id   int64
	user *users.User
	err  error
}

func (f *fakeLoader) LoadUser(ctx context.Context, id int64) (*users.User, error) {
	f.id = id
	return f.user, f.err
}

// fakeSession implements auth.Session for in-process middleware tests.
type fakeSession struct{ id int64 }

func (f fakeSession) UserID(c echo.Context) int64 { return f.id }

func TestRequireAuth_NoSession_RedirectsHTML(t *testing.T) {
	e := echo.New()
	mw := auth.RequireAuth(fakeSession{id: 0}, &fakeLoader{})
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("middleware err: %v", err)
	}
	if called {
		t.Error("downstream handler must not run")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/auth/google/login" {
		t.Errorf("location: got %q, want /auth/google/login", loc)
	}
}

func TestRequireAuth_NoSession_HXRequestReturns401(t *testing.T) {
	e := echo.New()
	mw := auth.RequireAuth(fakeSession{id: 0}, &fakeLoader{})
	h := mw(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/api/strategies", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestRequireAuth_InactiveUser_TreatedAsLoggedOut(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{user: &users.User{ID: 7, IsActive: false}}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("inactive user must not reach handler")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
}

func TestRequireAuth_ValidUser_AttachesToContext(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{user: &users.User{ID: 7, Email: "alice@example.com", IsActive: true}}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)

	var seen *users.User
	h := mw(func(c echo.Context) error {
		seen = auth.UserFromContext(c)
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	if seen == nil || seen.ID != 7 {
		t.Errorf("UserFromContext: got %+v, want id=7", seen)
	}
	if loader.id != 7 {
		t.Errorf("loader called with %d, want 7", loader.id)
	}
}

func TestRequireAuth_LoaderNotFound_TreatedAsLoggedOut(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{err: users.ErrNotFound}
	mw := auth.RequireAuth(fakeSession{id: 99}, loader)
	called := false
	h := mw(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("handler must not run when user is not found")
	}
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", rec.Code)
	}
}

func TestRequireAuth_LoaderError_PropagatesAs500(t *testing.T) {
	e := echo.New()
	loader := &fakeLoader{err: errors.New("boom")}
	mw := auth.RequireAuth(fakeSession{id: 7}, loader)
	h := mw(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/strategies", nil)
	rec := httptest.NewRecorder()
	err := h(e.NewContext(req, rec))
	if err == nil {
		t.Fatalf("expected error to be returned to Echo")
	}
}

func TestRequireAdmin_NonAdmin_Returns403(t *testing.T) {
	e := echo.New()
	called := false
	h := auth.RequireAdmin()(func(c echo.Context) error { called = true; return nil })

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	auth.SetUserOnContext(c, &users.User{ID: 7, IsAdmin: false, IsActive: true})

	if err := h(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("non-admin must not reach handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", rec.Code)
	}
}

func TestRequireAdmin_Admin_PassesThrough(t *testing.T) {
	e := echo.New()
	called := false
	h := auth.RequireAdmin()(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	auth.SetUserOnContext(c, &users.User{ID: 7, IsAdmin: true, IsActive: true})

	if err := h(c); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Error("admin handler must run")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

func TestRequireAdmin_NoUser_Returns403(t *testing.T) {
	e := echo.New()
	h := auth.RequireAdmin()(func(c echo.Context) error { return nil })

	req := httptest.NewRequest(http.MethodGet, "/admin/ingest/status", nil)
	rec := httptest.NewRecorder()
	if err := h(e.NewContext(req, rec)); err != nil {
		t.Fatalf("err: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", rec.Code)
	}
}
