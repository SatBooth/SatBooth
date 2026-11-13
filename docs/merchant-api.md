# The merchant API (private listener)

Fixed routes; every other path or method answers 404 whatever the credentials. Bodies are at most 16 KiB. Errors are
`{"error":"<code>"}` with a fixed code.

`POST /v1/orders` (key with `orders:write`):

    {"account_id": "u-123", "merchant_ref": "inv-2026-0042", "lightning_msat": 21000000, "onchain_sat": 21000, "expires_in": 3600}

`account_id` and `merchant_ref` are 1 to 64 characters of `[A-Za-z0-9._-]`. An order is priced in one of two ways,
never both (a request with `price` and an amount is 400 `mixed_pricing`):

- **Explicit amounts**, per rail; the customer pays one of them, and your app prices each:
  - `lightning_msat`: the Lightning amount in msat, a whole number of sats within the limits;
  - `onchain_sat`: the on-chain amount in sat, 1 to 2,100,000,000,000,000; it needs the `onchain` block;
  - `monero_piconero`: the Monero amount in piconero (10^12 per XMR), 1 to 9,007,199,254,740,991 (2^53 − 1, about
    9,007 XMR, so a JavaScript `JSON.parse` reads it exactly); it needs the `monero` block.

  Give any of them. An absent member means no such rail; a member that is present must be valid (`null` and `0` are
  refused).
- **A fiat price**, which needs the `rates` block:

      {"account_id": "u-123", "merchant_ref": "inv-2026-0043", "price": "10.00", "currency": "USD", "expires_in": 900}

  - `price`: a JSON string (never a number), 1 to 20 digits, optionally a point and 1 to 18 digits, above zero; no
    sign, exponent or space;
  - `currency`: 3 uppercase letters, one of `rates.currencies`;
  - `rails`: optional, 1 to 3 distinct names of `lightning`, `onchain` and `monero`. Without it the order gets
    Lightning, and on-chain when the `onchain` block is set; **never Monero, which `rails` must name** (so a bitcoin
    sale never depends on the XMR rate sources).

  SatBooth converts the price at order creation: it takes the median of the fresh rates of the configured providers
  (the currency per coin), applies `margin_percent`, and rounds up to a whole base unit. The bitcoin rails share one
  BTC quote: `sats = ceil(price × 100,000,000 × (1 + margin) / median)`, and both get the same sats
  (`lightning_msat` = sats × 1000, `onchain_sat` = sats). The Monero rail has its own XMR quote:
  `monero_piconero = ceil(price × 1,000,000,000,000 × (1 + margin) / median)`. The rounding never charges less than
  the price; the customer pays at most 1 sat (or 1 piconero) more. A converted amount outside a rail's bounds refuses
  the order with that rail's code (`invalid_lightning_msat` below `min_msat` or above `max_msat`, `invalid_onchain_sat`
  above the bitcoin supply, `invalid_monero_piconero` above 2^53 − 1); nothing is clamped and no rail is dropped. The
  rates are then locked with the order (see [Rules for fiat pricing](rules-fiat.md)).

`expires_in` is optional, 300 to 86400 seconds, default 3600: how long the order can be paid (for
on-chain, how long a payment first seen counts; see [Rules for on-chain payments](rules-onchain.md)). `merchant_ref` is unique across all
orders and makes the call idempotent: the same reference with the same account and amounts returns the existing order
(200, with its original expiry); with a different account or amount it is 409 `conflict`. For a priced order the
pricing is compared, not the amounts: the same reference with the same account, the same `price` **text, byte for
byte**, the same `currency` and the same rail set returns the existing order with its locked rate and amounts,
whatever the rate is now; anything else is 409 `conflict`. **A retry must send the same price text**: `"21.50"` then
`"21.5"` is a 409. A retry without `rails` takes the rail set from the configuration again, so after the operator
adds or removes the `onchain` block it is a 409 too (the `monero` block does not change that set). A priced repeat of an explicit order, or the reverse, is 409. A
priced request is looked up by its reference before any conversion: a repeat is 200 with the original order during a
rate outage, after the operator removed the currency from `rates.currencies` or the `rates` block, and when today's
rate would put a rail out of bounds; a reference already used by any other order is 409 `conflict` in those cases
too, before any other check of the request needs a rate or the configuration. A repeat of an order with an on-chain amount while the
`onchain` block is gone answers what `GET` answers for that order, 503 `onchain_unavailable`; likewise 503
`monero_unavailable` for an order with a Monero amount while the `monero` block is gone (an order with both gets
`onchain_unavailable` first). **If a response is lost,
retry the same `POST`**: it returns the order the first one created. A new order is 201.
`GET /v1/orders/{id}` (key with `orders:read`) returns the same object.

    {"id": "<32 hex>", "merchant_ref": "...", "account_id": "...", "lightning_msat": 21000000, "onchain_sat": 21000,
     "monero_piconero": 0,
     "state": "new", "created_at": "<RFC 3339>", "expires_at": "<RFC 3339>", "paid_at": null,
     "checkout_url": "https://pay.example.com/c/<token>",
     "lightning": {"state": "new"},
     "onchain": {"state": "waiting", "address": null, "required_confirmations": 1, "confirmations": 0,
                 "received_sat": 0, "confirmed_sat": 0, "late_sat": 0, "overpaid": false, "stale": false},
     "monero": null,
     "pricing": null}

`state` is the order's state, from all its rails, in this order of precedence:
- `paid`: any rail is paid (`paid_at` is then the earliest paid rail's time; it is null otherwise);
- `late`: not paid, and a payment first seen at or after the expiry is live on the on-chain or Monero rail (it waits
  for a person);
- `underpaid`: not paid, past the expiry, the Lightning side expired (or absent), and the live payments first seen
  before the expiry on a watched rail (on-chain or Monero) are short of the amount, or reach it but are still not
  confirmed enough when the 7-day watch window closes (see [The watch window](rules-onchain.md));
- `expired`: past the expiry, the Lightning side expired (or absent) and nothing was received on any watched rail;
- `new`: anything else, including an order past its expiry whose payment is still being confirmed or checked.

On an order with `onchain_sat` or `monero_piconero`, `underpaid` and `expired` come from its events, never from the clock alone: the order
reads one only once its `order.underpaid` or `order.expired` exists (or, after `order.paid`, an `order.reverted` that
no `order.paid_again` answers), and it never reads `expired` while an on-time payment is live. Once such an event
exists, an on-time payment live again (evicted and rebroadcast, or put back in the mempool by a reorg) reads
`underpaid`, not `new`, until `order.paid` or `order.paid_again`. So they lag the expiry
(and the window's end) by up to one pass, longer when a pass holds the event back because an on-time payment died
after its scan and its fee bump may be listed by the next one; until then the order reads `new`, with
`onchain.state` (or `monero.state`) `seen` or `waiting`. An order whose settle keeps failing past its window reads
`new` for good, and `ALERT: an on-chain order left its watch window without a final event; check it by hand` (or its
Monero twin) is the signal. On an order with both an on-chain and a Monero amount, a rail writes the final event only
when the other rail is not still being paid (its on-time payments reach its amount but are not confirmed enough) and
its view is not stale: a stale rail may hold money it cannot see. Such an order keeps reading `new` for as long as
the other rail stays stale, past its window too, where the left-the-window `ALERT` reports it.

`lightning` is null for an order without `lightning_msat`, `onchain` null for one without `onchain_sat`, and `monero`
null for one without `monero_piconero`; the amount of a missing rail reads 0. `lightning.state` is `new`, `paid` or
`expired`. A Lightning order also carries `lightning.paid_msat` (what its paid invoices received),
`lightning.refunded_msat` (what its settled refunds paid back) and `lightning.refund_open_msat` (what its claimed or
sent refunds hold), with or without the `payout` block, so a refund already paid stays visible after the block is
removed. In `onchain`:
- `state`: `waiting` (nothing seen), `seen` (seen, not yet confirmed enough), `paid`, `overpaid` (paid, with more than
  the amount), `late` (a late payment is live and the rail is not paid), `underpaid` and `expired`;
- `address`: null until the customer presses "Pay"; it is the merchant's own address, from the descriptor;
- `required_confirmations`: the tier of `onchain_sat`, which may be 0 (paid from the mempool; see "0-conf risk");
  `confirmations`: the fewest confirmations among the payments that count, and 0 while one of them is unconfirmed (so
  an order paid at a 0 tier can read `paid` with `confirmations` 0);
- `received_sat`: the live payments first seen before the expiry, confirmed or not; `confirmed_sat`: those recorded
  in the ledger at their tier and not reverted; `late_sat`: the live payments first seen at or after the expiry;
- `overpaid`: paid with more than the amount. `late_sat` and `overpaid` stay visible on a paid order: refund the extra;
- `stale`: true before the watcher's first good pass and when it has had no good pass for 3 poll intervals plus the
  last good pass's duration (the checkout page then says "Checking payment…"). It is also true for one order alone
  when, in the last pass that ran to its end, its settle failed or a call for one of its payments failed: that order
  was not brought up to date, the others were. An order whose payment waited only because bitcoind was still loading
  its mempool is not stale. The other values may be old while it is true.

These values come from SatBooth's store and ledger, never from a call to bitcoind at request time. While bitcoind does
not answer they keep their last values, and `stale` turns true. A Lightning-only order's `expired`, and an
`onchain.state` of `waiting`, can follow from the clock and SatBooth's last pass while a payment sits unseen: **re-read
the order until `onchain.stale` is false before you act on `expired`** (cancel the order or release its stock).

`monero` has the same members and meanings, in piconero, from the Monero watcher's store and ledger (never a call to
wallet-rpc at request time):

    "monero": {"state": "seen", "address": "8...", "required_confirmations": 1, "confirmations": 0,
               "received_piconero": 500000000000, "confirmed_piconero": 0, "late_piconero": 0,
               "overpaid": false, "stale": false}

- `address`: null until the customer presses "Pay with Monero"; it is a subaddress of the merchant's own wallet
  (`account_index`), made for this order alone;
- `required_confirmations`: the tier of `monero_piconero`, which may be 0 (paid from the tx pool); `confirmations`:
  the fewest among the payments that count, as Monero wallets count them (a payment in the wallet's last block has 1),
  from the wallet's own height at the last pass, never SatBooth's scan position, and 0 while one of them is in the
  pool;
- `received_piconero`, `confirmed_piconero` and `late_piconero`: as `received_sat`, `confirmed_sat` and `late_sat`. A
  payment its sender locked beyond the normal 10 blocks is late, whenever it was sent (see "Rules for Monero
  payments");
- `stale`: as `onchain.stale`, for the Monero watcher (no good pass for 3 poll intervals plus the last good pass's
  duration, or this order not brought up to date in the last pass), so it turns true while Monero watching is paused.
  **Re-read the order until `monero.stale` is false before you act on `expired`.**

`pricing` is on every response: `null` for an order with explicit amounts. For a priced order it is the quote
recorded at creation, never changed afterwards, with one rate per asset priced: `BTC` for the Lightning and on-chain
amounts, `XMR` for the Monero amount. **Its shape changed in plan 8** (before any release): plan 7's top-level `rate`
and `sources` moved into `rates`, keyed by asset.

    "pricing": {"price": "10.00", "currency": "USD", "margin_percent": "1.5", "quoted_at": "<RFC 3339>",
                "rates": {
                  "BTC": {"rate": "76025",
                          "sources": [{"provider": "coinbase", "rate": "76100", "at": "<RFC 3339>"},
                                      {"provider": "custom", "rate": "76050", "at": "<RFC 3339>"},
                                      {"provider": "kraken", "rate": "76000", "at": "<RFC 3339>"},
                                      {"provider": "yadio", "rate": "75950", "at": "<RFC 3339>"}]},
                  "XMR": {"rate": "250.5",
                          "sources": [{"provider": "bitfinex", "rate": "250", "at": "<RFC 3339>"},
                                      {"provider": "kraken", "rate": "251", "at": "<RFC 3339>"}]}}}

- `price` and `currency`: as the request sent them (`10.00` stays `10.00`);
- `margin_percent`: the operator's margin when the order was made, a decimal string; the same for every asset;
- `quoted_at`: when the order was priced (the oldest of its quotes' times). The times are UTC, in whole seconds;
- `rates`: one member per asset priced, only those the order's rails need. `rate` is the median of the sources,
  before the margin, in the currency per coin (for an even count, the mean of the two middle rates rounded down,
  here 76000 and 76050 for BTC). With `margin_percent` `"1.5"` this order (`rails` `["lightning","monero"]`) is
  `ceil(10.00 × 100,000,000 × 1.015 / 76025)` = 13,351 sats, `lightning_msat` 13351000, and
  `ceil(10.00 × 10^12 × 1.015 / 250.5)` = 40,518,962,076 piconero;
- `sources`: the fresh rates that made that asset's median, sorted by provider name; `at` is the provider's own time
  for its rate (Yadio and the custom API), or the fetch time (Kraken, Coinbase and Bitfinex).

The response never carries an invoice, payment hash or preimage. Send the customer to `checkout_url`. Other codes:
- 400 `invalid_json`, `invalid_account_id`, `invalid_merchant_ref`, `invalid_lightning_msat`, `invalid_onchain_sat`,
  `invalid_monero_piconero`, `missing_amount` (neither an amount nor a price), `onchain_disabled` (`onchain_sat`, or
  `"rails"` naming `onchain`, without the `onchain` block, also on a repeat of an existing explicit order; a priced
  repeat is 503 `onchain_unavailable`, see above), `monero_disabled` (the same for `monero_piconero`, `"rails"` naming
  `monero` and the `monero` block; a priced repeat is 503 `monero_unavailable`), `invalid_expires_in` or
  `body_too_large`;
- 400 for a priced request: `invalid_price` (not a positive decimal string, or `currency` without `price`),
  `invalid_currency` (not 3 uppercase letters, not in `rates.currencies`, or `price` without `currency`; and, for an
  order whose `rails` name `monero`, a currency the configured sources can never quote for XMR: an *unreachable* pair,
  see [Rules for fiat pricing](rules-fiat.md). It is permanent, with no `Retry-After`: do not retry it), `invalid_rails` (not a list
  of 1 to 3 distinct rail names, or `rails` without `price`), `mixed_pricing` (`price` with `lightning_msat`,
  `onchain_sat` or `monero_piconero`) and `fiat_disabled` (`price` without the `rates` block);
- 401 `unauthorized`; 403 `forbidden`; 404 `not_found`; 409 `conflict`; 500 `internal`;
- 503 `onchain_unavailable` (an order with an on-chain amount while the `onchain` block is not configured, for example
  after it was removed; retry once it is back), and `monero_unavailable` (the same for a Monero amount and the
  `monero` block);
- 503 `rate_unavailable`, with `Retry-After: 10`: no rate for a reachable pair: the fetcher has not filled its cache
  yet, fewer than `min_sources` providers answered within `max_age_seconds`, or they disagree by more than
  `max_spread_percent`; retry after `Retry-After` seconds; explicit orders still work. A priced order with Monero
  needs both its BTC rate (when it has a bitcoin rail) and its XMR rate.

The `known` class (an account with an order paid on any rail) is derived from SatBooth's own payment records for
the `account_id` you send. The limits apply to Lightning invoices only; an on-chain address or a Monero subaddress
takes no limit slot. Use a stable id per customer.
