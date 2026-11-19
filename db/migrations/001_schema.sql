-- Products: source of truth for price & stock. Valkey keeps a cached copy.
CREATE TABLE products (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    highlights     TEXT[]      NOT NULL DEFAULT '{}',
    image_url      TEXT        NOT NULL DEFAULT '',
    price          BIGINT      NOT NULL CHECK (price >= 0),
    original_price BIGINT      NOT NULL DEFAULT 0,
    -- stock:           units on hand that are not yet sold to an order (deducted when an order is created)
    -- available_stock: units a shopper can still reserve = stock - units held by in-flight checkouts
    -- CHECK constraints are the last line of defence against overselling.
    stock           INT         NOT NULL CHECK (stock >= 0),
    available_stock INT         NOT NULL DEFAULT 0 CHECK (available_stock >= 0 AND available_stock <= stock),
    flash_sale      BOOLEAN     NOT NULL DEFAULT FALSE, -- hot product: put the Valkey gate in front of the DB
    is_addon       BOOLEAN     NOT NULL DEFAULT FALSE,
    version        BIGINT      NOT NULL DEFAULT 1,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE coupons (
    code          TEXT PRIMARY KEY,
    description   TEXT        NOT NULL DEFAULT '',
    discount_type TEXT        NOT NULL CHECK (discount_type IN ('fixed', 'percent')),
    value         BIGINT      NOT NULL CHECK (value >= 0),
    min_subtotal  BIGINT      NOT NULL DEFAULT 0,
    usage_limit   INT,
    used_count    INT         NOT NULL DEFAULT 0,
    active        BOOLEAN     NOT NULL DEFAULT TRUE,
    expires_at    TIMESTAMPTZ
);

CREATE TABLE customers (
    id         BIGSERIAL PRIMARY KEY,
    email      TEXT        NOT NULL UNIQUE,
    name       TEXT        NOT NULL,
    phone      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Checkout draft: what the shopper submitted. Processed asynchronously by the worker.
CREATE TABLE checkout_drafts (
    id              TEXT PRIMARY KEY,
    idempotency_key TEXT        NOT NULL UNIQUE,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL, -- pending / processing / completed / failed
    attempts        INT         NOT NULL DEFAULT 0,
    error_code      TEXT,
    error_message   TEXT,
    error_details   JSONB,
    order_id        TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_checkout_drafts_status_updated ON checkout_drafts (status, updated_at);

-- Stock held by a checkout draft. held -> committed (order created) | released (checkout failed)
CREATE TABLE inventory_reservations (
    draft_id   TEXT        NOT NULL REFERENCES checkout_drafts (id),
    product_id BIGINT      NOT NULL REFERENCES products (id),
    quantity   INT         NOT NULL CHECK (quantity > 0),
    status     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (draft_id, product_id)
);
CREATE INDEX idx_inventory_reservations_held ON inventory_reservations (product_id) WHERE status = 'held';

CREATE TABLE orders (
    id             TEXT PRIMARY KEY,
    order_no       TEXT        NOT NULL UNIQUE,
    draft_id       TEXT        NOT NULL UNIQUE REFERENCES checkout_drafts (id), -- one draft can never create two orders
    customer_id    BIGINT      NOT NULL REFERENCES customers (id),
    status         TEXT        NOT NULL, -- pending_payment / paid / expired / refund_required
    subtotal       BIGINT      NOT NULL,
    discount       BIGINT      NOT NULL,
    shipping_fee   BIGINT      NOT NULL,
    donation       BIGINT      NOT NULL,
    total          BIGINT      NOT NULL,
    coupon_code    TEXT,
    payment_method TEXT        NOT NULL,
    shipping       JSONB       NOT NULL,
    invoice        JSONB       NOT NULL,
    note           TEXT        NOT NULL DEFAULT '',
    expires_at     TIMESTAMPTZ NOT NULL,
    paid_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_orders_status_expires ON orders (status, expires_at);

CREATE TABLE order_items (
    id         BIGSERIAL PRIMARY KEY,
    order_id   TEXT   NOT NULL REFERENCES orders (id),
    product_id BIGINT NOT NULL REFERENCES products (id),
    name       TEXT   NOT NULL, -- snapshot at order time
    unit_price BIGINT NOT NULL, -- snapshot at order time
    quantity   INT    NOT NULL,
    amount     BIGINT NOT NULL
);
CREATE INDEX idx_order_items_order ON order_items (order_id);

-- One order may have several payment attempts (e.g. card declined, then retry).
CREATE TABLE payments (
    id                  TEXT PRIMARY KEY, -- also used as merchant_trade_no sent to the gateway
    order_id            TEXT        NOT NULL REFERENCES orders (id),
    method              TEXT        NOT NULL,
    amount              BIGINT      NOT NULL,
    status              TEXT        NOT NULL, -- pending / succeeded / failed / expired
    provider_session_id TEXT        NOT NULL DEFAULT '',
    provider_txn_id     TEXT        NOT NULL DEFAULT '',
    payment_url         TEXT        NOT NULL DEFAULT '',
    failure_reason      TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_payments_order ON payments (order_id);
