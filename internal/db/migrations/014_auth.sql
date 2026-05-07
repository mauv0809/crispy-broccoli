-- +goose Up

CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    name            TEXT,
    is_admin        BOOLEAN NOT NULL DEFAULT FALSE,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at   TIMESTAMPTZ
);

CREATE TABLE auth_identities (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider     TEXT NOT NULL,
    provider_id  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_id)
);
CREATE INDEX auth_identities_user_id_idx ON auth_identities (user_id);

-- scs/postgresstore canonical schema.
CREATE TABLE sessions (
    token  TEXT PRIMARY KEY,
    data   BYTEA NOT NULL,
    expiry TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_expiry_idx ON sessions (expiry);

-- +goose Down
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS auth_identities;
DROP TABLE IF EXISTS users;
