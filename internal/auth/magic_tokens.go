package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTokenInvalid covers the consume path: token unknown, expired, or
// already used. Collapsed into one error so the handler can't leak which
// of those it was via timing or response shape.
var ErrTokenInvalid = errors.New("magic token invalid")

// MagicTokenStore persists single-use login tokens.
type MagicTokenStore interface {
	Insert(ctx context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error
	Consume(ctx context.Context, tokenHash []byte) (userID int64, err error)
	RecentCount(ctx context.Context, userID int64, since time.Time) (int, error)
}

type magicTokenRepo struct{ pool *pgxpool.Pool }

func NewMagicTokenStore(pool *pgxpool.Pool) MagicTokenStore { return &magicTokenRepo{pool: pool} }

func (r *magicTokenRepo) Insert(ctx context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO magic_tokens (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("magic_tokens insert: %w", err)
	}
	return nil
}

// Consume atomically marks a token as used and returns the bound user_id.
// The WHERE clause makes it race-safe: two concurrent callers can't both
// claim the same token, because the second UPDATE matches zero rows once
// consumed_at is set.
func (r *magicTokenRepo) Consume(ctx context.Context, tokenHash []byte) (int64, error) {
	const q = `
		UPDATE magic_tokens
		   SET consumed_at = NOW()
		 WHERE token_hash = $1
		   AND consumed_at IS NULL
		   AND expires_at > NOW()
		RETURNING user_id`
	var userID int64
	err := r.pool.QueryRow(ctx, q, tokenHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrTokenInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("magic_tokens consume: %w", err)
	}
	return userID, nil
}

func (r *magicTokenRepo) RecentCount(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM magic_tokens WHERE user_id = $1 AND created_at > $2`,
		userID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("magic_tokens count: %w", err)
	}
	return n, nil
}

// generateMagicToken returns (raw, hash). The raw token only ever leaves
// the process inside the email; the DB stores the hash so a leaked DB
// dump can't be replayed into active sessions.
func generateMagicToken() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// hashToken is the verify-side counterpart of generateMagicToken.
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
