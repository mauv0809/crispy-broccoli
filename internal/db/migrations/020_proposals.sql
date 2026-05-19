-- +goose Up
CREATE TABLE proposals (
    id                          BIGSERIAL PRIMARY KEY,
    portfolio_id                BIGINT NOT NULL REFERENCES portfolios(id) ON DELETE CASCADE,
    strategy_version_id         BIGINT NOT NULL REFERENCES strategy_versions(id) ON DELETE RESTRICT,
    generated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    market_value_at_proposal    NUMERIC(18,2) NOT NULL,
    capital_change              NUMERIC(18,2) NOT NULL DEFAULT 0,
    deploy_amount               NUMERIC(18,2) NOT NULL,
    picks                       JSONB NOT NULL,
    status                      TEXT NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','accepted','partially_accepted','skipped','expired')),
    resolved_at                 TIMESTAMPTZ,
    notification_sent_at        TIMESTAMPTZ,
    reminder_sent_at            TIMESTAMPTZ
);

CREATE INDEX proposals_portfolio_idx ON proposals(portfolio_id, generated_at DESC);
CREATE INDEX proposals_pending_idx
    ON proposals(portfolio_id)
    WHERE status = 'pending';
CREATE INDEX proposals_reminder_idx
    ON proposals(notification_sent_at)
    WHERE status = 'pending' AND reminder_sent_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS proposals;
