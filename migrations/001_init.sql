-- CrossVenue operational schema. Raw high-volume market events go to
-- recording files, NOT here; this schema stores operational state only.

CREATE TABLE IF NOT EXISTS orders (
    id              TEXT PRIMARY KEY,
    client_order_id TEXT NOT NULL,
    venue           TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    side            SMALLINT NOT NULL,
    type            SMALLINT NOT NULL,
    price           NUMERIC(38, 8) NOT NULL DEFAULT 0,
    quantity        NUMERIC(38, 8) NOT NULL,
    filled_quantity NUMERIC(38, 8) NOT NULL DEFAULT 0,
    avg_fill_price  NUMERIC(38, 8) NOT NULL DEFAULT 0,
    state           SMALLINT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (venue, client_order_id)
);

CREATE TABLE IF NOT EXISTS fills (
    id          BIGSERIAL PRIMARY KEY,
    order_id    TEXT NOT NULL REFERENCES orders(id),
    venue       TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    side        SMALLINT NOT NULL,
    price       NUMERIC(38, 8) NOT NULL,
    qty         NUMERIC(38, 8) NOT NULL,
    fee         NUMERIC(38, 8) NOT NULL,
    filled_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS fills_order_idx ON fills(order_id);

CREATE TABLE IF NOT EXISTS positions (
    venue     TEXT NOT NULL,
    symbol    TEXT NOT NULL,
    qty       NUMERIC(38, 8) NOT NULL,
    avg_cost  NUMERIC(38, 8) NOT NULL DEFAULT 0,
    PRIMARY KEY (venue, symbol)
);

CREATE TABLE IF NOT EXISTS balances (
    venue     TEXT NOT NULL,
    asset     TEXT NOT NULL,
    available NUMERIC(38, 8) NOT NULL,
    reserved  NUMERIC(38, 8) NOT NULL DEFAULT 0,
    PRIMARY KEY (venue, asset)
);

CREATE TABLE IF NOT EXISTS opportunities (
    id            TEXT PRIMARY KEY,
    symbol        TEXT NOT NULL,
    buy_venue     TEXT NOT NULL,
    sell_venue    TEXT NOT NULL,
    quantity      NUMERIC(38, 8) NOT NULL,
    buy_vwap      NUMERIC(38, 8) NOT NULL,
    sell_vwap     NUMERIC(38, 8) NOT NULL,
    gross_pnl     NUMERIC(38, 8) NOT NULL,
    fees          NUMERIC(38, 8) NOT NULL,
    slippage      NUMERIC(38, 8) NOT NULL,
    latency       NUMERIC(38, 8) NOT NULL,
    net_pnl       NUMERIC(38, 8) NOT NULL,
    edge_bps      BIGINT NOT NULL,
    detected_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS opportunities_detected_idx ON opportunities(detected_at);

CREATE TABLE IF NOT EXISTS engine_events (
    id           BIGSERIAL PRIMARY KEY,
    sequence     BIGINT NOT NULL,
    type         TEXT NOT NULL,
    venue        TEXT NOT NULL DEFAULT '',
    symbol       TEXT NOT NULL DEFAULT '',
    aggregate_id TEXT NOT NULL DEFAULT '',
    payload      JSONB NOT NULL DEFAULT '{}',
    occurred_at  TIMESTAMPTZ NOT NULL,
    recorded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS engine_events_type_idx ON engine_events(type, occurred_at);

CREATE TABLE IF NOT EXISTS system_state (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
