# Webhooks

SatBooth posts these events to each endpoint of the `webhooks` list. A webhook is a hint: after verifying it, read the
order from the API. **Events can arrive out of order** (each is retried on its own); **re-read the order on every
event** and act on its `state`, not on the event's type.

**Several endpoints.** `webhooks` lists 1 to 8 endpoints, each `{"name", "url", "secret_file"}` (the rules are in
"Configuration"). The shop, an accounting tool and a logger can each have one.

- **Every endpoint gets every event**, with the same body and the same signature format. There is no filter.
- **Each endpoint has a secret of its own**, and no two are equal. An endpoint verifies with its secret only (see
  below).
- **Each endpoint is delivered on its own**: its own attempts, retries, 24-hour limit and give-up line (see the end of
  this section). An endpoint that is down never delays another.
- **The receiver you had before this change is the endpoint `main`.** The attempts recorded before the change belong
  to `main`, so it continues where the single webhook stopped. `satbooth-setup` writes one endpoint, `main`; add more
  to `satbooth.json` by hand.
- **A new endpoint** (a name the data directory has not seen) is sent the events created in the 24 hours before its
  first delivery pass, and nothing older. It catches up by itself, in one burst. A receiver added today therefore gets
  the last 24 hours, including events from before it was added. The first pass is stamped by the host's clock, and an
  endpoint is admitted the events from the later of 7 days and 24 hours ago and 24 hours before the first pass (or
  before now, if the clock ran ahead at the first pass); an event it has already been tried with stays until delivered
  or given up. An endpoint first seen while the clock ran behind can be sent more than 24 hours of events (never more
  than seven days beyond the retry window), and one first seen while the clock ran ahead can miss an event it never
  tried if the service was down (or the event otherwise had no attempt) until it fell below that floor
  (`docs/threat-model.md` §11 has the detail).
- **A removed endpoint** is no longer delivered to. Its recorded attempts stay.
- **An endpoint added again** under a name it had before is the same endpoint. It continues where it stopped, as after
  downtime: what it was sent is not sent again, what it gave up on stays given up, and an event it never tried gets
  its attempts.
- **A renamed endpoint is a new endpoint**: the old name is a removed one and the new name is new.

| Event | When | How often |
|---|---|---|
| `order.paid` | the order first reads paid, on any rail | once per order |
| `order.expired` | past the expiry, with nothing paid or counted on any rail | once per order |
| `order.underpaid` | past the expiry, the Lightning side expired (or absent), with on-chain or Monero payments that count but fall short of the amount; or, when the watch window closes, with on-time payments that reach the amount but are not confirmed enough | once per order |
| `order.late` | an on-chain or Monero payment that does not count (first seen at or after the expiry, or, for Monero, locked by its sender) has its tier's confirmations and is recorded (a person decides) | once per order |
| `order.reverted` | a reorg (or, on a 0 tier, a payment gone from the mempool or tx pool) took away on-chain or Monero payments, and the order no longer reads paid | each time, with `seq` 1, 2, … |
| `order.paid_again` | after `order.reverted` with `seq` n, the order reads paid again | once per reversal, with that reversal's `seq` |

An order gets at most one of `order.expired` and `order.underpaid`, at the first check after its expiry, or at the
window's end for an on-time payment of the whole amount still unconfirmed then; later changes show only in the API
(so an order that got `order.expired` and whose on-time payment came back into the mempool reads `underpaid` past the
window with no second final event). `order.reverted` and `order.paid_again` follow the order's combined state: a reorg under
an order that is still paid by Lightning, by its other payments or by its other watched rail sends no event (it logs
an `ALERT`). A Lightning payment recorded while another rail's payment of the order is being reversed comes after that
reversal, and the order then gets `order.paid_again`. There is never a second `order.paid`.

**`order.paid` can follow `order.expired`** by up to 24 hours on Lightning (about 48 hours if SatBooth restarted in
between): Lightning nodes accept a payment for a while after an invoice's expiry, so a customer can pay an invoice
after SatBooth has expired its order. The order then reads `paid`; handle it (ship, or refund). On-chain,
`order.paid` can also follow `order.expired` or `order.underpaid` while the address is watched (7 days after the
expiry): a payment first seen before the expiry that dropped out of the mempool and was mined later still counts. It
can also come after the window, for an on-time payment still tracked then (see [The watch window](rules-onchain.md)). The same holds
for Monero. On Lightning `order.paid` is final; on-chain and in Monero a reorg can still take it back
(`order.reverted`).

The body is JSON with five fields, `id` (the event id), `type`, `seq` (1, except for `order.reverted` and
`order.paid_again`), `order_id` and `created_at` (Unix seconds). The id is the first 32 hex characters of SHA-256 over
`satbooth/event/v1`, the type and the order id, with `/` and the decimal `seq` appended when `seq` is 2 or more, all
without separators: so the same event always has the same id.

For a priced order the body has a sixth member, `pricing`, with the members of the API's `pricing` object
(`price`, `currency`, `margin_percent`, `quoted_at`, and `rates` keyed by asset, each with its `rate` and
`sources`), the times in Unix seconds like `created_at` (wrapped and shortened here; the real body is one line, and
the assets are in alphabetical order). Plan 8 changed this shape, as the API's: plan 7's top-level `rate` and
`sources` moved into `rates`.

    {"id":"…","type":"order.paid","seq":1,"order_id":"…","created_at":1800000000,
     "pricing":{"price":"10.00","currency":"USD","margin_percent":"1.5","quoted_at":1799999995,
                "rates":{"BTC":{"rate":"76025","sources":[{"provider":"coinbase","rate":"76100","at":1799999990},…]},
                         "XMR":{"rate":"250.5","sources":[{"provider":"bitfinex","rate":"250","at":1799999990},…]}}}}

An order with explicit amounts gets the five fields and nothing else, byte for byte as before, so the test vector
below is unchanged. The quote is read from the store for each attempt, and an event is never sent without its price.
While the store cannot be read, delivery waits and is retried. A stored quote that does not read back (a row written
past SatBooth) holds back that order's events alone: nothing is sent or recorded for them, they stay pending, and the
other events are delivered as usual. SatBooth logs `ALERT: a webhook event is held back: its order's stored quote is
malformed` with the event id once per event per run, whatever the number of endpoints (each later pass logs a line
without `ALERT` at the debug level); after a restart it is logged once again.

The request carries

    SatBooth-Signature: t=<unix seconds>,v1=<64 lowercase hex>

To verify a request:

1. Read the raw body bytes; do not re-serialise them.
2. Parse the header: exactly `t=<digits>,v1=<64 hex>`.
3. Compute HMAC-SHA256 over the bytes `t`, a dot `.`, and the body, with the key being **the raw secret bytes**. The
   secret file holds the secret as hex: decode it first. A receiver that keys the HMAC with the hex text verifies
   nothing.
4. Compare with `v1` in constant time.
5. Reject if `t` is more than 5 minutes from your clock, either way (each attempt, retries included, is signed
   afresh). Run NTP.
6. Deduplicate on the event `id`, and answer 2xx once the event is recorded.

Test vector (the secret is made up for this example; never use it):

| | |
|---|---|
| secret file contents | `0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0` (32 bytes as hex) |
| `t` | `1800000000` |
| body | `{"id":"daef60e344d3989032713d2dedfb9a74","type":"order.paid","seq":1,"order_id":"c4a1f09e5b7d2386a0e9d1c7f3b5a842","created_at":1800000000}` |
| header | `t=1800000000,v1=5519510e2c192e0985132af9c71bdae25815c472bc4114e0d91e8c3cb512d78e` |

The id is the event id of that order's `order.paid`, by the rule above. For the same input, keying the HMAC with the
hex text instead of the decoded bytes gives `05fe8a839cd9e7919c285beef699ea3dba5f4d96efc7a000cc31da61394ce85c`, which
is wrong. Check your code against the vector with openssl:

    printf '%s' '1800000000.{"id":"daef60e344d3989032713d2dedfb9a74","type":"order.paid","seq":1,"order_id":"c4a1f09e5b7d2386a0e9d1c7f3b5a842","created_at":1800000000}' \
      | openssl dgst -sha256 -mac HMAC -macopt hexkey:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0

Delivery is at-least-once, for each endpoint. An idle deliverer starts a pass when an event is stored, so it is posted
within about a second; passes of one endpoint are at least a second apart. A pass lasts as long as its requests, which
go one at a time, each with its 10 second timeout: a slow receiver delays its own later events (an event stored while a
pass runs waits for the next one) and no other endpoint's. Should the store's signal be missed, an idle deliverer looks
again at most 30 seconds after its last pass ended; retries follow the steps below. Any 2xx is success; any other
status, a timeout (10 s), a connection failure or a redirect is a failed attempt (redirects are not followed, and no
proxy from the environment is used). After a failure the next attempt comes after 10 s, then 20 s, 40 s and so on,
doubling up to 1 hour, until 24 hours after the event was created. The last attempt is the first one at or after that
time; if it fails, SatBooth logs `ALERT: webhook delivery abandoned after 24 hours` with the event id and the endpoint's
name, and never tries that endpoint again for that event. When two endpoints give up on one event, that is two lines and
two operator alerts, each with its `endpoint`. The line is not lost to a crash: the last failed attempt and a mark
"abandoned, not yet announced" are written together, and SatBooth removes the mark after it has logged the line. A pass
logs the line of every marked event, so after a crash or a restart between the attempt and the line it is logged when
SatBooth starts again, once per run (a crash between the line and the removal of the mark can repeat it once, after the
restart; it is never lost). The mark stays until the endpoint is configured again if the endpoint was removed from the
configuration in the meantime, and the line is then logged. Events abandoned before the upgrade to the version that
added this mark get no line. An event whose 24 hours ended more than 7 days before SatBooth last started gets no final
attempt and no such log line. If the mark cannot be removed after the line was logged, SatBooth logs
`webhook give-up mark could not be removed; it will be retried` (no data from the error), goes on delivering, and the
mark is removed by a later pass or announced once more after a restart.

The same event can arrive more than once. If the receiver answers 2xx and SatBooth cannot record it (a store error),
SatBooth keeps the answer in memory and retries only the record, without posting again and without marking the event
abandoned, until the record succeeds (or the event has left the endpoint's delivery window for good, 7 days after its
24 hours). This also holds for the last attempt and for the first attempt after a long downtime, which are made after
the 24-hour limit. A restart between your 2xx and its record forgets that: the event is posted again, with the same
id. Deduplicate on the `id`.
