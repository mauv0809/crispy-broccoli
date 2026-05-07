-- +goose Up

CREATE TABLE magic_tokens (
    token_hash   BYTEA       PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at   TIMESTAMPTZ NOT NULL,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX magic_tokens_user_recent_idx ON magic_tokens (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS magic_tokens;
