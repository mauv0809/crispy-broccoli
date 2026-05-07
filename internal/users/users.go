package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID          int64
	Email       string
	Name        string
	IsAdmin     bool
	IsActive    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

var ErrNotFound = errors.New("user not found")

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// GetByID returns the user with the given primary key. ErrNotFound when missing.
func (r *Repository) GetByID(ctx context.Context, id int64) (*User, error) {
	const q = `SELECT id, email, name, is_admin, is_active, created_at, last_login_at
	           FROM users WHERE id = $1`
	return r.scanOne(r.pool.QueryRow(ctx, q, id))
}

// GetByEmail returns the user with the given email. ErrNotFound when missing.
func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	const q = `SELECT id, email, name, is_admin, is_active, created_at, last_login_at
	           FROM users WHERE email = $1`
	return r.scanOne(r.pool.QueryRow(ctx, q, email))
}

// Upsert inserts a user or no-ops if the email already exists. Returns the
// resulting (or existing) user. is_admin and is_active on conflict are NOT
// changed; that lets the env-seeded admin survive a startup that runs after
// an admin has manually demoted themselves.
func (r *Repository) Upsert(ctx context.Context, email, name string, isAdmin bool) (*User, error) {
	const q = `
		INSERT INTO users (email, name, is_admin, is_active)
		VALUES ($1, $2, $3, TRUE)
		ON CONFLICT (email) DO UPDATE
		   SET name = COALESCE(NULLIF(EXCLUDED.name, ''), users.name)
		RETURNING id, email, name, is_admin, is_active, created_at, last_login_at`
	return r.scanOne(r.pool.QueryRow(ctx, q, email, name, isAdmin))
}

// SetActive flips is_active. Used by --disable-user.
func (r *Repository) SetActive(ctx context.Context, email string, active bool) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET is_active = $1 WHERE email = $2`, active, email)
	if err != nil {
		return fmt.Errorf("set active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastLogin sets last_login_at = NOW() for the given user.
func (r *Repository) TouchLastLogin(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("touch last_login_at: %w", err)
	}
	return nil
}

// EnsureIdentity finds the user linked to (provider, providerID). If the
// identity row does not yet exist but a user with `email` does, the
// identity is created and that user is returned (admin pre-provisioning
// path). If no user exists for that email, ErrNotFound is returned.
func (r *Repository) EnsureIdentity(ctx context.Context, provider, providerID, email string) (*User, error) {
	// 1) identity already linked?
	const idLookup = `
		SELECT u.id, u.email, u.name, u.is_admin, u.is_active, u.created_at, u.last_login_at
		FROM auth_identities ai
		JOIN users u ON u.id = ai.user_id
		WHERE ai.provider = $1 AND ai.provider_id = $2`
	if u, err := r.scanOne(r.pool.QueryRow(ctx, idLookup, provider, providerID)); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// 2) admin pre-provisioned by email?
	u, err := r.GetByEmail(ctx, email)
	if err != nil {
		return nil, err // ErrNotFound bubbles
	}

	// 3) link the identity to that user
	_, err = r.pool.Exec(ctx,
		`INSERT INTO auth_identities (user_id, provider, provider_id) VALUES ($1, $2, $3)`,
		u.ID, provider, providerID)
	if err != nil {
		return nil, fmt.Errorf("insert identity: %w", err)
	}
	return u, nil
}

func (r *Repository) scanOne(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return &u, nil
}
