# Checkout Demo

A runnable local checkout system that covers the critical transaction flow end to end:
product → checkout page → async order creation → third-party payment (mocked) → callback → result page.

**Stack:** Go 1.24 (stdlib `net/http`), PostgreSQL 16, Valkey 8, Kafka 3.8 (KRaft), vanilla HTML/JS. The frontend is embedded into the Go binary.

## Quick start

```bash
make up          # docker compose up --build -d  (first start ~30s, Kafka is the slowest)
open http://localhost:8080
make logs        # follow app logs
make reset       # wipe data; DB is re-seeded on next start
```

To run the Go app on the host instead: `make infra`, then `make run`. The flash-sale gate sync is a separate one-shot command: `make gatesync` (in compose it runs as the `gate-sync` service every 30s).

> **Upgrading from an older checkout:** the schema changed (accounts, draft ownership). The schema only runs on a fresh volume, so run `make reset` once before `make up`.

**Tests:** `make test` runs the unit tests. `scripts/smoke-test.sh` runs the end-to-end scenarios below against a running stack (needs `curl` and `jq`), including login, ownership and admin checks.

**Inspecting data with a GUI** (TablePlus, DBeaver, Postico, pgAdmin, DataGrip, ...):

| | Host | Port | User / password | Database |
|---|---|---|---|---|
| PostgreSQL | `localhost` | `5432` | `checkout` / `checkout` | `checkout` |
| Valkey | `localhost` | `6379` | none | `0` |

Useful Valkey keys: `gate:{product_id}` (flash-sale gate), `session:{token}` (login sessions).

**Accounts:** browsing is public; checkout, orders and payment need an account. Register on `/login.html`. The demo admin account is `admin@example.com` / `admin1234`, created on startup from `ADMIN_EMAIL` / `ADMIN_PASSWORD`.

| URL | What |
|---|---|
| http://localhost:8080 | Product list with cart |
| http://localhost:8080/login.html | Login / register (email + password) |
| http://localhost:8080/admin.html | Demo admin (admin account only): change price / on-hand stock, toggle flash sale; shows available vs. reserved (opens in its own tab from the top bar) |
| http://localhost:8080/checkout.html?items=1:2,2:1 | Checkout page; `product_id:quantity` pairs come from the query string (`?product_id=1&qty=1` also works) |
| http://localhost:9090 | Mock payment gateway; a separate server that simulates a third party |

## Demo scenarios

| # | Scenario | How |
|---|---|---|
| 1 | Happy path | Add several products to the cart (or open a checkout URL) → credit card `4242 4242 4242 4242` → result page shows **付款完成** |
| 2 | Card declined, then retry | Use card `4000 0000 0000 0002` → result page shows **付款未完成** → 重新付款 |
| 3 | Price changed while the shopper is on the page | Open checkout, change the price in the admin tab (`/admin.html`), submit → modal lists the change, page refreshes, shopper re-confirms |
| 4 | Not enough stock / sold out | Set stock to 1 in admin and choose qty 2, or open product 3 (sold out). With several products, the reservation is all-or-nothing: nothing is held if one item is short |
| 5 | Last unit race | Set stock = 1, submit from two tabs → exactly one wins, the other gets an immediate **已售完**. Works for both flash-sale products (Valkey gate) and normal products (DB row lock) |
| 6 | Coupons | `RETURN500` (min 3,000), `SAVE10` (10% off), `ONCE100` (single use; second order is rejected) |
| 7 | ATM / AFTEE | Pick the method; the gateway shows a virtual account and a "simulate transfer" button |
| 8 | Double submit | Double-click submit or retry after a network error → same `Idempotency-Key`, so only one order |
| 8b | Reload while processing | After submit the URL gets `&draft=<id>`; reloading (or coming back with the browser's back button) resumes polling that draft instead of submitting again. If processing takes over 60s the page says "still processing" and only offers "keep checking", never a second submit |
| 9 | Unpaid order expiry | Set `PAYMENT_TTL_SECONDS=60`, leave an order unpaid → it expires and stock is returned |

## Architecture

```mermaid
flowchart LR
  B[Browser<br/>checkout / result pages] -- REST --> API
  B -- redirect --> GW[Mock Payment Gateway :9090]
  GW -- signed callback (HMAC) --> API

  subgraph app[Go process]
    API[HTTP API :8080] --> SVC[App services]
    C[Kafka consumer group<br/>3 members] --> SVC
    J[Jobs: reclaim drafts,<br/>expire unpaid orders] --> SVC
  end
  GS[gate-sync cronjob<br/>cmd/gatesync] -- realign gates --> VK
  GS -- read available_stock --> PG

  SVC -- source of truth --> PG[(PostgreSQL<br/>products + available_stock, reservations,<br/>coupons, customers, drafts, orders, payments)]
  SVC -- product cache +<br/>flash-sale gate --> VK[(Valkey)]
  SVC -- checkout.requested --> K[(Kafka)]
  K --> C
  SVC -- create payment session --> GW
```

Code layout (layered: interfaces → app → domain, with infra behind it):

```
cmd/main.go                      wiring, graceful shutdown
cmd/gatesync/                    one-shot flash-sale gate sync (run by a cron runner)
config/                          env config
db/migrations/                   schema + seed (run by postgres initdb)
internal/domain/{def,model}      error codes, statuses, pricing & validation (pure, unit tested)
internal/app/                    AuthService, ProductService, CheckoutService, OrderService, PaymentService
internal/app/adapter/            service input / output types
internal/infra/database/repo     PostgreSQL (pgx)
internal/infra/cache             product cache, flash-sale gate (Lua), sessions, login limiter
internal/infra/producer          Kafka producer
internal/infra/paymentgateway    gateway client + signature verification
internal/interfaces/web          HTTP handlers, auth middleware
internal/interfaces/consumer     Kafka consumer
internal/interfaces/job          background jobs
internal/mockgateway             the fake third-party payment provider
web/static                       frontend
```

## Transaction flow

```mermaid
sequenceDiagram
  autonumber
  participant B as Browser
  participant A as API
  participant V as Valkey
  participant P as Postgres
  participant K as Kafka
  participant W as Worker
  participant G as Gateway

  B->>A: POST /api/checkouts (Idempotency-Key, items + prices seen, expected_total)
  A->>A: format validation
  opt cart has flash-sale products
    A->>V: Lua: take units from the gate (all-or-nothing)
  end
  A->>P: TX: insert draft (pending) + reserve available_stock + insert reservations
  alt not enough stock (gate or DB)
    A->>V: give gate units back (if taken)
    A-->>B: 409 ITEMS_CHANGED (sold out / insufficient)
  else reserved
    A->>K: publish checkout.requested {draft_id}
    A-->>B: 202 {draft_id}
  end
  loop every ~1s
    B->>A: GET /api/checkouts/{draft_id}
  end
  K->>W: message
  W->>P: claim draft (pending → processing)
  W->>P: TX: lock products & coupon, re-check price/coupon/total,<br/>deduct stock, reservation → committed, upsert customer,<br/>insert order, draft → completed
  alt rejected (price changed / coupon / amount)
    W->>P: TX: reservation → released, available_stock += qty, draft → failed
    W->>V: refill flash-sale gate
  end
  B->>A: POST /api/orders/{id}/payments
  A->>G: create payment session (signed)
  A-->>B: payment_url
  B->>G: pay
  G->>A: POST /api/payments/callback (HMAC signed, retried)
  A->>P: payment → succeeded, order → paid (idempotent)
  G-->>B: redirect to /result.html
  B->>A: GET /api/orders/{id} (poll until callback is applied)
```

### Authentication and ownership
- **Email + password.** Passwords are hashed with bcrypt. Emails are lower-cased.
- **Session.** Login creates a random 256-bit token stored in Valkey (`session:{token}`, 7 days) and set as an `HttpOnly; SameSite=Lax` cookie.
  - HttpOnly: JavaScript cannot read the token, so an XSS cannot steal it.
  - Logout deletes the token server-side, so it is revoked immediately (a JWT could not be).
- **Login hardening.**
  - 10 failed logins per email in 15 minutes returns `429`.
  - An unknown email still runs a bcrypt comparison, so response time does not reveal which emails are registered.
  - The `?next=` redirect after login only accepts same-site paths (no open redirect).
- **CSRF.** `SameSite=Lax` stops browsers from sending the cookie on cross-site POST/PUT. Authenticated state-changing requests must also be `application/json`, which a plain HTML form cannot send.
- **Access rules.**

  | Who | APIs |
  |---|---|
  | anyone | products, checkout options, register / login |
  | logged-in shopper | coupon preview, submit checkout, draft status, orders, start payment |
  | admin role | `PUT /api/admin/products/{id}` |
  | gateway | payment callback, authenticated by HMAC signature instead of a session |

- **Ownership (no IDOR).**
  - Drafts store `customer_id`, and orders belong to the draft's account. The buyer is always the logged-in account, never the email in the request body.
  - Reading a draft or order, or starting a payment, checks the owner. Someone else's resource returns **404, same as a missing one**, so ids cannot be probed.
  - Idempotency keys are unique per account, `UNIQUE (customer_id, idempotency_key)`, so reusing another user's key never returns their draft.

### Status machines

- **Draft:** `pending → processing → completed | failed`. Transient errors put it back to `pending`; a job re-publishes it.
- **Order:** `pending_payment → paid | expired`. `refund_required` means the payment arrived after the order had expired.
- **Payment:** one order can have several attempts, each `pending → succeeded | failed | expired`.

## Key design decisions

### 1. The price or stock changed after the shopper loaded the page
The client sends the unit price it displayed and the `expected_total` the shopper confirmed. The server **never silently applies a new price**. It rejects with `ITEMS_CHANGED` (per-item reason: `PRICE_CHANGED`, `INSUFFICIENT_STOCK`, `SOLD_OUT`, `NOT_FOUND`) or `AMOUNT_MISMATCH`. The page re-fetches products, updates itself, and asks the shopper to confirm again.

### 2. Async order creation with Kafka
`POST /api/checkouts` only persists a draft and returns `202`. The heavy, contended work (row locks, stock deduction) runs in a consumer group, so traffic spikes are smoothed instead of piling up DB connections in request handlers.

- Topic `checkout.requested` has 3 partitions, keyed by `draft_id`.
- Offsets are committed **after** handling (at-least-once delivery).
- Duplicate messages are harmless:
  - the worker claims a draft with `UPDATE … WHERE status='pending'`;
  - `orders.draft_id` is `UNIQUE`.
- A failing draft never blocks a partition. It is put back to `pending`, the offset is committed, and the reclaim job re-publishes it.
- The **draft row in Postgres is the source of truth.** If a publish fails or a worker dies, the reclaim job re-publishes drafts that are `pending` for more than 10s or `processing` for more than 60s.

### 3. Stock reservation: PostgreSQL for everything, a Valkey gate for flash sales
Kafka only orders messages within a partition, and with several consumers the completion order is not the submit order anyway; a single partition would kill parallelism. So "who gets the last unit" is decided **synchronously at submit time**, not by the queue.

Each product has two numbers:

| Column | Meaning | Changes when |
|---|---|---|
| `stock` | on hand, not yet sold to an order | order created (−), unpaid order expired (+) |
| `available_stock` | what shoppers can still buy = `stock` − units held by in-flight checkouts | checkout submitted (−), checkout failed or order expired (+) |

**Every product.** `POST /api/checkouts` inserts the draft and runs `UPDATE products SET available_stock = available_stock - qty WHERE id = ? AND available_stock >= qty` for every item **in the same transaction**, plus a row in `inventory_reservations` (`held → committed | released`).

- One source of truth, and no window where a draft exists without its units (or the opposite).
- Rows are locked in product-id order, so overlapping carts cannot deadlock.
- If one item is short the whole transaction rolls back (all-or-nothing).
- Releasing the units and marking the draft failed are also one transaction.
- `CHECK (available_stock >= 0 AND available_stock <= stock)` guarantees no overselling.

**Flash-sale products** (`products.flash_sale`). Thousands of shoppers updating the *same row* would queue on its row lock, hold DB connections, and slow down the whole database. So these items first pass a **Valkey gate** (`gate:{id}`, atomic all-or-nothing Lua decrement):

- Only about `available_stock` requests ever reach the DB; the rest get an instant "sold out".
- The gate is a **load shedder, not the source of truth**. If it drifts (a crash between gate and DB, a Valkey restart), the worst case is rejecting a few shoppers early or letting a few extra requests through to the DB, which rejects them. It can never oversell.
- **Initialising a gate.** The app does not build gates at startup, so it never waits on (or fails because of) the gate sync. A missing gate is created on first use: the Take script reports **every** missing gate in the cart at once (after the first missing one it only scans for more missing ones), then the server reads their `available_stock` in one query, creates them with `SETNX`, and retries once.
- **Realigning.** A standalone cronjob (`cmd/gatesync`, the `gate-sync` compose service, every 30s) sets every gate to `available_stock`. If Valkey is down, the gate fails open and the DB decides.
- **Memory limit.** Valkey runs with `maxmemory 256mb` and `volatile-lru`: only keys with a TTL (product cache, sessions) can be evicted. Gates have no TTL, so they are never evicted; if memory is still full, gate writes fail and checkout falls back to the DB.

Why not Valkey for everything? It would mean two sources of truth to reconcile on every path (crash windows, restarts, drift). That cost is only worth paying for the few hot products where row-lock contention is the real bottleneck.

### 4. DB + cache writes for product updates
`PUT /api/admin/products/{id}`:

1. Update the row inside a DB transaction.
2. Write Valkey **before commit**. If the cache write fails, roll back.
3. If the commit fails after the cache was written, delete the cache key.

So "DB and cache both succeed, or nothing changes". Reads use cache-aside.

### 5. Payment
- The gateway client signs requests. Callbacks are verified with HMAC-SHA256 and the amount is re-checked.
- Callbacks are idempotent: the payment row is locked, and payments already in a final state are acknowledged without side effects. The gateway retries on any non-2xx response.
- A payment that arrives after the order expired becomes `refund_required` instead of being lost.
- The order stays `pending_payment` after a failed attempt, so the shopper can retry until `expires_at`. After that a job expires the order and returns stock and coupon usage.

### 6. Idempotent submit
The browser sends an `Idempotency-Key` per confirmed payload. A replay with the same key returns the same draft and does not reserve stock again.

Once the server accepts a checkout, the page writes the `draft_id` into its URL. A reload, the back button from the payment page, or a 60s polling timeout all resume **the same draft**: the page never offers a second submit while a draft is in flight, because that would create a second order and a second reservation. When a resumed draft is already completed, the page goes to the order page rather than silently opening a new payment session.

## API

| Method | Path | Notes |
|---|---|---|
| GET | `/api/products`, `/api/products/{id}` | `available_stock` = what can be bought; `stock` = on hand; `flash_sale` |
| GET | `/api/checkout/options` | shipping regions & fees, payment methods |
| POST | `/api/auth/register` | `{email, password, name}` → `201`, sets the session cookie |
| POST | `/api/auth/login` | `{email, password}` → sets the session cookie; `401` wrong credentials, `429` too many failures |
| POST | `/api/auth/logout` | revokes the session |
| GET | `/api/auth/me` | current user, or `401` |
| POST | `/api/coupons/preview` | 🔒 `{code, subtotal}` → discount (display only) |
| POST | `/api/checkouts` | 🔒 header `Idempotency-Key`; returns `202 {draft_id}` or `400` / `409` |
| GET | `/api/checkouts/{id}` | 🔒 owner only; `{status, order_id?, error?}` |
| GET | `/api/orders/{id}` | 🔒 owner only; order + latest payment status |
| POST | `/api/orders/{id}/payments` | 🔒 owner only; creates or reuses a gateway session → `{payment_url}` |
| POST | `/api/payments/callback` | gateway → shop, `X-Signature` header |
| PUT | `/api/admin/products/{id}` | 🔒 admin only: `{price?, stock?, flash_sale?}`; changing `stock` moves `available_stock` by the same delta |

🔒 = requires a logged-in session (`401` otherwise; `403` for non-admins on admin APIs).

Errors look like `{"error": {"code": "ITEMS_CHANGED", "message": "...", "details": {...}}}`.

## Configuration

All settings are environment variables (see `config/config.go`); the defaults work for local runs.

| Variable | Default | Notes |
|---|---|---|
| `HTTP_ADDR` / `GATEWAY_ADDR` | `:8080` / `:9090` | API and mock gateway listen addresses |
| `DATABASE_URL` | `postgres://checkout:checkout@localhost:5432/checkout?sslmode=disable` | |
| `VALKEY_ADDR` | `localhost:6379` | |
| `KAFKA_BROKERS`, `KAFKA_TOPIC`, `KAFKA_PARTITIONS`, `KAFKA_GROUP_ID` | `localhost:29092`, `checkout.requested`, `3`, `checkout-worker` | |
| `CONSUMER_COUNT` | `3` | consumer group members in the process |
| `APP_PUBLIC_URL`, `APP_INTERNAL_URL` | `http://localhost:8080` | redirect target / callback target for the gateway |
| `GATEWAY_PUBLIC_URL`, `GATEWAY_API_URL`, `GATEWAY_API_KEY`, `GATEWAY_SECRET` | demo values | gateway URLs, API key, HMAC secret |
| `PAYMENT_TTL_SECONDS` | `900` | unpaid orders expire after this |
| `SESSION_TTL_HOURS` | `168` (7 days) | login session lifetime |
| `COOKIE_SECURE` | `false` | set `true` when served over HTTPS |
| `ADMIN_EMAIL`, `ADMIN_PASSWORD` | `admin@example.com`, `admin1234` | bootstrap admin, created on startup. Demo only: use a secret store in production |

## Known limitations / next steps

- **Auth extras.** Possible additions: email verification and password reset (they need a mail service), rotating the session id on privilege change, and re-reading the role from the DB instead of the session snapshot (today a demoted admin keeps the role until the session expires). "My orders" list page.
- **Product cache race.** A reader could write a stale product after a writer updates it. A fix is a version-guarded `SET` (Lua comparing `version`). Correctness is unaffected: price is re-checked in the DB transaction, and stock is never served from the cache.
- **Reservation timeout.** A held reservation lives until its draft completes or fails (the reclaim job keeps retrying). Adding `expires_at` to `inventory_reservations` would also cap how long units can be held.
- **Display stock under extreme load.** Product pages read stock from the DB (MVCC reads never wait on locks). At flash-sale scale they could read the gate counter instead.
- **Valkey Cluster.** The multi-key gate script would need hash tags such as `{gate}` to keep keys in one slot.
- **Outbox pattern.** Publishing after the draft insert relies on the reclaim job. An outbox table, or CDC with Debezium, makes it exactly-once-publish.
- **Notifications.** Emit `order.created` and `order.paid` events for email, invoice, and fulfilment as separate consumer groups.
- **Observability.** Add metrics (draft latency, queue lag, payment success rate) and tracing across the API, Kafka, and worker.
