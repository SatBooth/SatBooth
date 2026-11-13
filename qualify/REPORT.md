# Hub qualification: measured values

Run: `scripts/qualify.sh` against Alby Hub v1.24.6 (source pinned at getAlby/hub 921003b; all phases pass; the timings
and counts below are from the run against the v1.24.4 image; its source is cited at getAlby/hub a1dfc61, two commits
after the v1.24.4 tag), two hubs on a regtest network with no internet, SatBooth's relay in front of each, Bitcoin Core
32.1, and (plan 8) Monero v0.18.5.4. Values come from the `MEASURED` lines of the test log.
Times are wall-clock on one development host and only show orders of magnitude.

## Environment and channel

| What | Measured |
|---|---|
| `POST /api/start` to `running: true`, no internet | 1.2 s (both hubs); the startup channel-suggestion call fails without blocking |
| Block to the merchant's on-chain balance | 2.0 s |
| Blocks until the 1,000,000 sat channel is `active` | 6 (confirmations 1 to 6 after each block; `confirmationsRequired` 6; `active` and `status` "online" at block 6, no extra wait) |
| 300,000 sat top-up payment, merchant to SatBooth | 0.2 s |
| Hub restart after bootstrap (stop to exit) | 0 to 1 s, exit code 0 |

## Protocol

| Check | Measured |
|---|---|
| 13194 content, invoice connection | `make_invoice make_hold_invoice settle_hold_invoice cancel_hold_invoice lookup_invoice list_transactions get_info get_budget notifications`; tags `encryption p notifications extensions` |
| 13194 content, payout connection | `pay_invoice pay_keysend multi_pay_invoice multi_pay_keysend lookup_invoice get_info get_budget`; tags `encryption p extensions` (paired with `pay_invoice`, `lookup_invoice` and `get_info`, as the payout link asks; `get_info` reports the same methods) |
| 13194 seen by a client | at once after a hub restart (the hub republishes at startup). In an earlier run the relay came up after the hubs had created their connections and the first 13194 took 55.6 s (the hub's retry backoff), within the 3 minute bound |
| `get_info` without the `get_info` scope | `network` and `block_height` are absent (empty and 0); `methods` and `notifications` are present (measured before plan 14; no SatBooth connection lacks the scope now) |
| `get_info` with the `get_info` scope | the invoice connection, the payout connection and the `info` connection all state `network` "regtest"; block height 108 (invoice and `info`) |
| `get_info` notifications, invoice connection | `payment_received payment_sent hold_invoice_accepted`; payout connection: none |
| Customer payment to notification (invoice connection) | 0.08 to 0.12 s from the start of the customer's payment (hub's own invoice: 0.12 to 0.17 s) |
| `pay_invoice` of 3,000 sat, direct channel | 0.09 to 0.15 s; `fees_paid` 0 msat; merchant's notification 0.08 to 0.13 s after the start |
| `hold_invoice_accepted` after the payout starts | 0.09 to 0.14 s |
| Unknown request author (stranger) | no response; the client gave up at its 15 s timeout (15.0 s); the relay answered OK (no refusal) |
| Cancel of a hold invoice to the payout call returning | 0.3 s |

## Budget

- Before any payout: `used_budget` 0, `total_budget` 50,000,000 msat, `renewal_period` "never".
- After the 3,000 sat payout (fee 0): `used_budget` 3,000,000 msat = amount + fee.
- With a 15,000 sat hold-invoice payout pending: `used_budget` 18,150,000 msat = 3,000,000 + 15,000,000 + 150,000.
  The reserve was 1% of the amount (150,000 msat; the 10 sat floor is 10,000 msat).
- A pending 500 sat hold payout raised `used_budget` by 510,000 msat: the 10 sat floor (a plain 1% would be 5 sat).
- An invoice of (remaining budget in sat) - 1, which fits without its reserve, is refused with `QUOTA_EXCEEDED`, so the
  reserve counts toward the limit.
- After the hold invoice is cancelled, the row is `failed` and `used_budget` is back at 3,000,000 msat.

## WalletError codes and messages as observed

| Situation | Code | Message |
|---|---|---|
| Second `pay_invoice` for a settled invoice | `INTERNAL` | `this invoice has already been paid` |
| `pay_invoice` with an amount that differs from the invoice's | `INTERNAL` | `amount cannot be specified for an invoice that already has an amount` |
| `pay_invoice` of an expired invoice | `INTERNAL` | `this invoice has expired` |
| Payout above the remaining budget (with or without the reserve) | `QUOTA_EXCEEDED` | `Your app does not have enough budget remaining to make this payment. Please review this app in the connections page of your Alby Hub.` |
| `pay_invoice` on the invoice connection | `RESTRICTED` | `This app does not have the pay_invoice scope` |
| Payout of a hold invoice the receiver cancelled | `INTERNAL` | `received payment failed event: RecipientRejected` |

The merchant's row for the expired invoice reads `expired` (synthesized past `expires_at`).
A payment refused by the checks above moves no budget and leaves the merchant's invoice unsettled.
After the second `pay_invoice` the merchant's balance rose by exactly the invoice amount once.

## List page size

| Request | Rows | Plaintext of the response |
|---|---|---|
| `list_transactions` limit 20, unpaid included, 100-character descriptions | 20 | 19,720 bytes (NIP-44 limit 65,535) |
| limit 50 (the hub's maximum), same wallet | 30 (all that existed) | 28,631 bytes |

About 950 to 990 bytes per row. A page of 20 uses about 30% of the NIP-44 limit; 50 rows of this size would be about 49,000 bytes.

## Differences from expectations

1. `GetInfo` on a connection without the `get_info` scope does not report the network: the hub withholds `network` and
   `block_height` without that scope (the invoice link of plan 4 had none). Since plan 14 the SatBooth links, and the
   suite's invoice and payout connections, hold `get_info`, and both state `regtest`; the `info` connection stays for
   the crash phases.
2. `UnknownKey`: the relay config refuses one wallet key in two pairs, so the stranger cannot be paired with the invoice
   connection's wallet key. The stranger is paired with the wallet key of a separate live app (`decoy`) whose client
   key is another one; the hub looks the app up by the request's author, so the stranger is unknown to it in the same way.
   The relay refuses the decoy app's own 13194 (`restricted: p tag does not match the author's pair`: the event names the
   decoy client, the pair names the stranger), and the hub retries it with growing backoff for as long as it runs. So the
   suite has no event that proves the hub is subscribed on that wallet; the silence is consistent with the author lookup
   but not proven by it.

## Payouts in flight

Setup of each crash phase: the merchant makes a 2,000 sat hold invoice, SatBooth's payout connection pays it, the merchant
sees the HTLC (`hold_invoice_accepted` 0.08 to 0.12 s after the call), SatBooth's row is `pending` and the merchant's row
`accepted`. The phase ends there. The hub is then stopped (`docker compose stop -t 60`) or killed (`kill -s KILL`), started
again, its relay is recreated, and the After phase runs once `GET /api/info` says `running`. Source: getAlby/hub a1dfc61
`transactions/transactions_service.go:301-470, 900-970`, `lnclient/ldk/ldk.go:640-712`.

| Step | Graceful stop | Kill (SIGKILL) |
|---|---|---|
| Time to stop | 0 to 1 s, exit code 0, never killed at the 60 s (the hub logged SIGTERM, "LDK stop node succeeded" 10 to 15 ms later) | immediate |
| Rows right after the restart (hub API, both hubs) | SatBooth: outgoing `pending`; merchant: incoming `accepted` | same |
| `lookup_invoice` right after the restart | outgoing, `pending`, answered on the first attempt (0.0 s) | same |
| Second `pay_invoice` of the same invoice | answered at once: `INTERNAL` "there is already a payment pending for this invoice" (the hub's own check; the hub log has "Failed to create DB transaction"). No new row, no second HTLC | same |
| LDK refused a duplicate? | not reached: the hub's check stopped the retry before LDK was asked | same |
| After `settle_hold_invoice` | settle accepted at the first attempt; the payout row read `settled` 2.0 s later (the polling interval) | same |
| Final rows | SatBooth: 1 outgoing row, `settled`, fee 0; merchant: 1 incoming row, `settled` | same |
| Balances (stabilised) | merchant +2,000,000 msat (once); SatBooth -2,000,000 msat (amount + fee 0) | same |

Safety property: held in both phases, from both hubs' records. The merchant was credited once (one settled incoming row,
balance +invoice amount) and SatBooth's hub paid once (one settled outgoing row, balance -(amount + fee)).

What was not observed. The path that was feared (the hub marks the row `failed` when the node shuts down mid-payment,
so a retry passes the hub's check and only LDK's duplicate check could stop it) did not occur. By the source
(`ldk.go:662-712`, `transactions_service.go:449-460`) a shutdown can make `SendPaymentSync` return an error and
`markPaymentFailed` run. In the hub log after SIGTERM there is no "Failed to send payment" and no "Marked transaction as
failed" line; the process exited about 15 ms after the signal. That the exit came before the write is an inference from
this, not something the log states. The row stayed `pending` and, after the restart, the LDK payment success event
settled it. Note that the post-restart `lookup_invoice` (and the hub API list) pass through the hub's own reconciliation
of unsettled rows (`transactions_service.go:663`, `checkUnsettledTransaction`), so `pending` is the state after that pass,
not the raw row.

Repeats. `QUALIFY_CRASH_REPEATS` was used once, with 6 graceful stops and 3 kills, at the first revision of the test,
which paid 5,000 sat per round (the amount was lowered to 2,000 sat afterwards). Every round: `pending` after the
restart, the same retry answer, safety held. The fourth kill round did not start its payout: the hub answered the payout
request with "Your app does not have enough budget remaining to make this payment" (QUOTA_EXCEEDED; hub log line "Failed
to create DB transaction"), because the payout connection's 50,000 sat budget was spent (earlier tests 3,000 sat, 9
rounds of 5,000 sat plus 1% reserves each while pending). Count of stops and kills that ran to the end of their After
phase: 8 graceful stops (1 + 6 in the 5,000 sat runs, 1 in the final 2,000 sat run) and 5 kills (1 + 3 + 1), 13 in all
in the first runs; a later run added 1 stop and 1 kill, 15 in all. The failed fourth kill round is not counted. All 15
read `pending` after the restart; none read `failed`. So whether LDK in the pinned binary refuses a duplicate payment,
and the `failed` then `settled` move of fact 2, remain unverified (a race with the process exit that this setup did not
hit).

Recommendation for plan 9, refunds (not an observation): do not rely on the row being `pending` after a stop. After any restart,
resolve a payout by `lookup_invoice`, and never pay again after the hub reported `failed` without looking the hash up first.

### Hub request dedup across a hub restart

A `get_info` request (10 minute `expiration`, the `info` connection) is answered once before the stop or kill and saved. After
the hub restarted and its relay was recreated (empty window; the hub resubscribes with no `since` and an empty pool cache; the
hub's database persists), the same event is published again. The relay accepts it (`OK true`), the hub sends no answer with
that request id within 15 s, and `scripts/qualify.sh` counts the hub log line "Event already processed" (Warn, `nip47/event_handler.go:57-66`)
before and after each After phase and requires exactly one new line. That line proves the hub received the replay and refused
it by its database dedup. Observed: 0 before and 1 after the graceful-stop phase, 1 before and 2 after the kill phase.

### Relay loss (`relay-satbooth` stopped, started again with an empty window)

| Check | Observed |
|---|---|
| The hub answers again | 5.3 to 5.6 s after the relay came back (the hub's reconnect), on the first `get_info` |
| Lost notification | the customer paid a SatBooth invoice through the merchant hub while the relay was down; after it came back no `payment_received` arrived (client connected before and after); `lookup_invoice` says `settled` |
| Lost invoice response | a `make_invoice` request was published and its response never read; the relay restart discarded it. `list_transactions` (unpaid) shows the stray invoice (1), a resend succeeds (2 invoices with the description afterwards) |

Differences from what was expected: the real client's in-flight `MakeInvoice` ending in `ErrNoResponse` was not
reproduced against the live relay, because the hub answers in about 0.1 s and no restart can be timed into that gap. The
stray invoice was made by publishing the request on a raw connection that never subscribed, which loses the response the
same way. The relay-loss phase is split in three (`TestRelayLossBefore`, `TestRelayLossDuring` while the relay is
stopped, `TestRelayLossAfter`) because the customer has to pay while the relay is down.

An earlier version replayed a request after a relay-only restart. That check proved nothing and was dropped: after a dropped
relay the hub's client library resubscribes with `since` set to the time it noticed the drop, and SatBooth's relay applies
`since` to live events, so the old event may never have reached the hub. The dedup is now tested after a hub restart (above).

## Hub behaviours checked

| Point | Status | Where |
|---|---|---|
| `pay_invoice` re-sent after a payout the hub marked FAILED at shutdown; LDK DuplicatePayment; FAILED to SETTLED | Partly. The FAILED case at shutdown never occurred (15 stops and kills read `pending`); a hub-reported `failed` for an unroutable payment is observed in plan 9's `TestRefundFailed` (Lightning refunds, below). The retry of a `pending` payout is refused by the hub's own check (INTERNAL "there is already a payment pending for this invoice"). LDK's duplicate check was never reached: unverified. FAILED to SETTLED: unverified | Payouts in flight |
| LDK payment timeout / fail time with the pinned binary | Not measured. The hold windows used here are too short to observe a 50 s timeout, and measuring it needs a separate failing-route setup; plan 5 keeps the 70 s client timeout margin | none |
| `lookup_invoice` of an outgoing payment | `pending` observed (crash phases), `settled` observed (PayInvoice), `failed` observed (BudgetReserve, after the hold invoice was cancelled). Not observed: failed-then-retried (two rows, same hash) and an outgoing row past expiry (the expired invoice of the Expired test was refused before any outgoing row existed; only the merchant's incoming row read `expired`) | Protocol, Payouts in flight |
| Notification delivery when the relay drops a 23197; after a hub restart | First half observed: a payment made while `relay-satbooth` was down produced no notification afterwards (no retry when the test looked: right after the hub's first answered `get_info`, 5.3 to 5.6 s after the relay came back, and again after one `lookup_invoice`; a longer wait was not measured), and `lookup_invoice` says `settled`. Second half (queue after a hub restart): not measured by any phase | Relay loss |
| `get_budget` while a payout is pending and after it settles | Observed: pending 15,000 sat hold: used = amount + 1% (18,150,000 msat); the 10 sat floor applies below 1,000 sat; after settle used = amount + fee, with fee 0 on the direct channel (so "reserve replaced by the actual fee" is shown only for fee 0); after a cancel the budget returns to its start | Budget |

## Lightning end to end (plan 5)

The whole SatBooth service runs in-process in the `qualify` container, started as `cmd/satbooth` starts it
(`service.ReadConfig`, `service.Lock` with `memlock.Protect`, `Config.CheckFiles`, `service.Run`), against
`relay-satbooth` and the bootstrap's invoice connection. The merchant's hub plays the customer (`POST /api/payments`).
The `qualify` service has `ulimits: memlock: -1`: Docker accepted it and every start logged "memory locked; core
dumps disabled", so no `allow_swap` fallback is used. Each phase ends with the safety check, run after the service has
stopped, on the service's own ledger and store plus both hubs' rows: one ledger transfer (the id derived from the
payment hash, the order's amount) and one successful `order.paid` delivery per paid order, one request at the signature
verifying receiver (`webhook.Verify`), none of either for an unpaid order, no undelivered event, and the merchant
account holding exactly the paid orders' msat. A log line with `ALERT` fails the phase.

Amountless claimant invoices are not exercised and SatBooth refuses them at claim (`invalid_invoice`): getAlby/hub
`921003b` `transactions/transactions_service.go:180-181` refuses to make an invoice under 1 sat, so neither hub here
can issue one and the amountless path cannot be qualified.

| Phase | Observed |
|---|---|
| `TestLightningPay` | order created, "pay" 0.03 s to an invoice (reused when pressed twice), `qr.svg` a well-formed SVG (17 KB); customer's payment call 0.30 s; order read `paid` 0.41 s after the payment started (limit 10 s); `order.paid` at the receiver 0.50 s later, verified; an unpaid second order stayed `new` with no transfer and no event |
| `TestLightningReplay` | paid in 0.24 s; a second service on the same data read the order `paid`, no second transfer or event; the hub refused the customer's second payment of the invoice (HTTP 500 "this invoice has already been paid"); pressing pay on the paid order made no invoice |
| `TestLightningExpireBefore` / `After` | order expiry 300 s, invoice 15 minutes. Past the order's expiry the order still read `new`, until the invoice's expiry plus the 2 minute grace: 1,020 s after the invoice (979 s after the second service started); then the closing lookup, `expired` and one `order.expired` (verified, once). The NWC lookup read the invoice not settled (the test observes it as the closure's reason, `lookup`), while the hub's own REST `GET /api/transactions/{hash}` still read `pending` at that time (the test logs that row's state; observed during these runs) |
| `TestLightningRelayCut` | service up through an in-process TCP proxy to `relay-satbooth`; invoice made; the proxy cut (every live connection closed, new ones refused); the customer paid through the merchant hub (its own relay, not the proxy). For 25 s while cut: order `new`, invoice still on the page, no `ALERT` line, no `order.paid`, and the poller's lookups failed (1 completed failing lookup in the window, the next in flight: a failing lookup holds the loop for the client's 10 s timeout). Proxy restored: `paid` 5.8 s later (the client's reconnect and AUTH, then a lookup; the notification was lost while cut), no invoice closed, one transfer, one event, one settled row on each hub |
| `TestLightningRelayLossBefore` / `During` / `After` | invoice made, service stopped, `relay-satbooth` stopped, customer paid (0.16 s, the merchant hub needs no SatBooth relay), relay started, new service: `paid` 5.7 s after the start (1.5 s in an earlier run; it follows the hub's reconnect to the recreated relay, 5.3 to 5.6 s above), one transfer, one event |
| `TestLightningRestartBefore` / `After` | invoice made, service stopped, customer paid while it was down (SatBooth's ledger and store held no transfer or event at that point), new process and service on the same data: `paid` 0.2 s after the start, one transfer, one event |

What the two relay-loss scenarios prove. `RelayLoss*`: a service that starts after the relay came back with an empty window
finds a payment made while the relay was down, by lookup (nothing was waiting for it). `RelayCut`: a running service whose
connection is cut neither closes the invoice nor changes the order while it cannot reach the hub, and recovers by itself
after the connection returns. `RelayCut` needs the relay to accept the proxy's URL in the client's AUTH (checked byte for
byte), so the script gives `relay-satbooth` the proxy's URL as `client_relay_url` for that phase (`RelayCutPrepare`,
`RelayCutRestore`, each followed by recreating the relay).

Notification against poll. The notification subscription was live in `TestLightningPay`, but with two open invoices a
plain poll round (5 lookups a second) is as fast, so the path is not told apart from outside; the service logs no line for
it. The poll path alone is what `RelayLossAfter` (5.7 s, bounded by the hub's reconnect) and `RestartAfter` (0.2 s)
measure. The notification overtaking a slow poll is covered with the fake wallet (lnpay `TestNotificationOvertakesSlowPoll`).

Late payment after the closing lookup is not exercised live: the paying hub refuses to pay an expired invoice
(observed: the "`pay_invoice` of an expired invoice" row of the wallet errors above, `INTERNAL` "this invoice has
expired", logged as `MEASURED wallet_error label=expired`; the harness now prints the code and the message's length,
not its text), so no payment
reaches SatBooth's hub after the expiry. The late path is covered with the fake wallet (lnpay `TestLateSettle*`).
Whether the receiving hub accepts a late settle at all, and why no live phase reproduces it, is in "The late Lightning
payment (plan 10)" below.

Limits of the setup, stated plainly:
- One `go test` process per phase. The empty-window relay restart phases (`RelayLoss*`) therefore cannot keep the service up
  while `relay-satbooth` restarts: they prove recovery by lookup of a service that starts after the relay is back.
  Relay loss seen by a running service is `TestLightningRelayCut` (below).
- `TestLightningReplay` does not make the hub report a payment twice. The second pass is a restarted service that finds the
  invoice open with its transfer already in the ledger (`reconcileInv` skips the lookup and `completePaid` is idempotent).
  The races between a notification and a poll recording the same payment are covered with the fake wallet
  (lnpay `TestPaidExactlyOnce`, `TestPaymentRecordedConcurrentlyOnce`).
- The lost-response case of plan 4 (a `make_invoice` whose response is lost) is covered by the lnpay tests with the fake
  wallet (`TestPayLostResponse`). The live relay answers in about 0.1 s, so no restart can be timed into that gap (see
  Relay loss above).
- The expiry phase needs the invoice's 15 minutes plus the grace in real time. `TestLightningExpireBefore` runs early and
  `TestLightningExpireAfter` last, which saves only the 90 s or so the phases in between take; the whole script takes about
  20 minutes (1,230 s in the final run). `ExpireAfter` fails if it starts too late to observe an order that is past its
  expiry while its invoice is not yet closable, and if it never sees the order `new` in that window.

## On-chain end to end (plan 6)

The same service, with the on-chain rail, runs in-process in the `qualify` container against the regtest bitcoind (Bitcoin
Core 32.1), through SatBooth's own RPC user: `rpcauth` plus `rpcwhitelist` with exactly the twelve runtime methods and
`rpcwhitelistdefault=0` (`dev/regtest/bitcoind/bitcoin.conf`; the test password guards nothing). The operator's side is
the test's setup, with the full user: a blank, `disable_private_keys` descriptor wallet `satbooth-watch` and
`importdescriptors` active (Ruling F). All on-chain phases share one environment (`/state/onchain`, one data directory),
so the address index is continuous. Every phase ends with the safety check over every order made so far (`orders.json`),
after the service has stopped: events in the store against the expected range per type (seq contiguous, `order.paid` at
most once), each event delivered with exactly one successful attempt and the receiver of this process seeing exactly the
deliveries made in it; for every output the forward and reversal transfers of every epoch, paired, with the net per
address equal to the merchant's keyed wallet's `getreceivedbyaddress` at the order's depth; the ledger accounts equal to
the sums of those transfers; every `ALERT` line expected in its exact number, and no other. A phase that cannot observe its
property fails (`poll` with a bound, `Errorf`); nothing logs and passes.

The log check counts every record at ERROR or WARN by message, not only the ALERT lines: `settle failed`, `scan failed`, `track failed`,
`bitcoind call failed for an output`, `on-chain status could not be read` and anything else fail a phase that did not expect them. In the
full run no phase logged any such record beyond its expected ALERTs.

**Reuse that a deployment must not do.** The keyed wallet `merchant` and the watch-only wallet `satbooth-watch` share the
`/0/*` chain here only so the test can confirm receipts with `getreceivedbyaddress`. `docs/bitcoind.md` forbids exactly this for a
deployment (a dedicated account). The keyed wallet never issues an address of its own.

The regtest descriptors (wpkh and tr, public data of a throwaway wallet) are in `descriptor/testdata/vectors.json`. They are
a snapshot of one run: every run creates a new wallet.

| Phase | Observed |
|---|---|
| `TestOnchainSetup` | Core 32.1's wpkh and tr external descriptors (`[fp/84h/1h/0h]tpub.../0/*`, `[fp/86h/1h/0h]...`) parse with `descriptor.Parse`; `getdescriptorinfo` of the canonical form: `isrange` true, `issolvable` true, `hasprivatekeys` false; the service's startup check passes against the imported wallet |
| `TestOnchainPay` | three orders, one per tier (100,000 sat / 1 confirmation, 2,000,000 sat / 3, 12,000,000 sat / 6); the page shows the address and "Send exactly"; paid from `miner`; blocks mined one at a time with the API read after each: "Seen, N of M confirmations" on the page and `confirmations` N in the API until the tier block, `paid` at the tier block and never before; `order.paid` once each. The mixed order's served `qr.svg` is byte for byte `qr.SVG(PaymentURI(address, amount, invoice))` (550-byte URI, limit 1,200; SVG saved as `/state/onchain/unified.svg`) |
| `TestOnchainRBF` | see the RBF section |
| `TestOnchainReorg` | see the reorg section |
| `TestOnchainPartial` | 60 % then 40 % (each mined once): `seen` with `confirmed_sat` 60,000, then `paid`, `confirmed_sat` 100,000, not overpaid; 120 %: `paid`, `overpaid` true, one overpaid `ALERT`, the paid page says the merchant will refund the extra |
| `TestOnchainLateBefore` / `After` | order with `expires_in` 300, nothing paid; once expired it reads `expired`, one `order.expired`; a payment after the expiry reads `late` (not `paid`, at any poll), `order.late` once, one late `ALERT`. The 60 % order of `Partial` (also 300 s) reads `underpaid` (one `order.underpaid`, one `ALERT`) and, after a 40 % top-up, `late` with `late_sat` 40,000 and one `order.late` |
| `TestOnchainRestartBefore` / `After` | an order of the 3-confirmation tier paid and mined once; the service stops; 2 blocks mined while it is down; the new process reads it `paid` within the bound of its first pass (see the timing note below), one forward transfer, one `order.paid` |
| `TestOnchainIndexBefore` / `After` | see the index section |
| `TestOnchainWhitelist` | as `satbooth`: `sendtoaddress`, `createwallet`, `importdescriptors` and `stop` answer HTTP 403 (a plain-text answer, not JSON-RPC); `getblockchaininfo` works, and so does `getmempoolinfo` (the 11th method), with a boolean `loaded`; and `gettxspendingprevout` with `mempool_only` (the 12th method) answers HTTP 200 with one element, and no `spendingtxid`, for a made-up outpoint; `getnewaddress` on the `miner` wallet also succeeds: the whitelist is per method, not per wallet, recorded and not relied on. The full `regtest` user is unaffected (`rpcwhitelistdefault=0`) |

### MEASURED

Paid latency per tier (poll interval 2 s; the watcher polls bitcoind with the satbooth user):

| Tier | send to `seen` | tier block to `paid` |
|---|---|---|
| 1 confirmation | 1.94 s | 2.53 s |
| 3 confirmations | 1.93 s | 2.65 s |
| 6 confirmations | 1.93 s | 2.52 s |

Both are bounded by the 2 s poll plus one pass. Other timings: second 40 % part mined to `paid` 2.1 s; overpaid block to
`paid` 3.5 s; restart: see the next paragraph; late payment: the second order (40 % top-up) read `late` 2.0 s after its block; the first late order (full amount)
already read `late` while its transaction was in the mempool, so its "0.02 s" is not a block-to-late time, it only says the state
had not changed at the block (a late output counts as `late_sat` from first sight, before it is mined); the unified QR SVG is 24.5 KB.

Reorg: a double spend mined (2 blocks) after `invalidateblock` of the paid block: the order reads `new` / `waiting` and
`order.reverted` seq 1 is delivered 1.8 s later (1.0 s from the `invalidateblock` when the watcher polls in between, the
third reorg case of `TestOnchainReorg`). That case then: the transaction returns to the mempool, the watcher reverts the
payment (`order.reverted` seq 1, the reversal `ALERT`), the next block gives `order.paid_again` seq 1 and the
`paid_again` `ALERT` 3.1 s after that block. The plain reorg (`invalidateblock`, 2 blocks, the transaction mined again):
end state `paid` 1.9 s after the blocks; in this run no pass fell between the invalidation and the new block, so no
event (the safety check accepts none, or `reverted` seq 1 together with `paid_again` seq 1).

Restart timing. `Run` makes its first pass at once (`onchain/watch.go` `Run`), and that pass settles the order: step 3
refreshes with `gettransaction` every stored output that is inside its order's watch window and not yet settled
(`store.WatchedTXOs` leaves out the outputs in `onchain_settled`, in any process), paced at `PollRate` (default 5 a
second, one ticker tick per output, `trackAll`), and step 4 settles the touched orders in the same pass. An output is
recorded as settled only when the same pass refreshed it at `SettledDepth` and its order settled cleanly, so a restart
no longer re-polls outputs that are already final. `TestOnchainRestartAfter` asserts that the first pass alone settled
the order: with P = watched outputs / `PollRate`, a second pass starts at max(P, poll interval) and would settle at
about max(P, poll interval) + P, so the bound is 1.5 P + the measured start time + 0.5 s from the launch, and the test
fails as unobservable if that bound is not below the second pass. In the last run (MEASURED onchain=restart): 8 watched
outputs, P 1.6 s, `paid` 1.80 s after the launch, bound 3.13 s, a second pass would have settled at about 3.60 s. A
deployment's restart catch-up grows with its unsettled outputs only.

### RBF

`bumpfee` keeping the output: the original transaction A reads `confirmations` 0 in the watch wallet until B is mined, then
-1 with `walletconflicts` 1; B reads 1. `gettransaction` of B does not exist in the watch wallet before it is mined
("Invalid or non-wallet transaction id", code -5): in this plan 6 run, read right after `bumpfee`, the watch-only wallet
had not learned the replacement from the mempool before the block. The plan 6.1 runs read the same right after `bumpfee`,
but once the wallet had processed the replacement (still unconfirmed) both entries listed `walletconflicts` 1 (see "Plan
6.1 follow-ups"): the wallet does learn a replacement from the mempool, after a delay. Here the watcher marked A dead and B
mined (store: 2 stored outputs, 1 dead, 1 mined; the order was
`paid` once, received 100,000 sat, never 200,000). `listsinceblock` of the watch wallet shows A as `receive` with 0
confirmations before and only B with 1 after; `removed` is empty. `bumpfee` with the `outputs` option: **accepted by Core
32.1** (so the hand-built `sendrawtransaction` fallback of the test helper was not used). With it the customer's output is
taken away: the order goes back from `seen` to `waiting` (received 0) before any block; after the block A reads -1 in the
miner wallet; the order stays `new`, no forward, no event (store: 1 stored output, dead).

### Reorg

`listsinceblock` from the invalidated block's hash with `include_removed`: `removed` lists the payment's transaction (1 entry);
from its parent's hash, nothing. The original transaction reads `confirmations` -2 after the double spend was mined 2
deep. `reconsiderblock` of the invalidated block: the active chain is unchanged (the chain with the double spend is longer),
the order still `new` / `waiting`, no new event.

### Index (3 kills instead of 100)

After 3 `SIGKILL`s of bitcoind, three new orders got three distinct indexes strictly above the recorded one (recorded
14, new 15 to 17), and `getaddressinfo` of each shows `hdkeypath` ending `/0/<index>`: the wallet's `next_index`
survived. The hazard: re-importing the same descriptor (with its current range, a smaller range is refused) and without
`next_index` moved the wallet's `next_index` back to 14, not to 0: the import's rescan moved it past the last *paid* address
(`MarkUnusedAddresses`); the three new orders' addresses stayed unpaid. The service then refused to start
("the on-chain startup check failed") with one `ALERT: wallet next_index is behind SatBooth's record`. Re-import with
`next_index` = the record + 1 (18, range end 1017): the service started and issued an index above the record.

**Observed: a `SIGKILL` of bitcoind loses the chain unless it is flushed.** In a first run, three
kills in a row, 10 minutes after the blocks were mined, brought bitcoind back at height 0 (the block index and chainstate
are flushed on a timer, and regtest blocks never reach that point): the hubs, the miner wallet and the ledger's
confirmations all pointed at blocks that no longer existed, and the service logged the "behind its headers" and re-anchor
`ALERT`s. The script therefore forces a flush with `gettxoutsetinfo` (which calls `ForceFlushStateToDisk`, `src/rpc/blockchain.cpp:1114` at v32.1) before each
kill and fails if the height after the restart is lower; the three kills then left the chain at height 128 each time. The
wallet's own database (SQLite) needed no such help.

### `tr` and `listdescriptors`

`getnewaddress "" bech32m` three times on a watch-only wallet with the merchant wallet's `tr` descriptor imported:
`hdkeypath` `m/86h/1h/0h/0/0`, `/1`, `/2` (observed; `ismine` true, `parent_desc` the imported descriptor).
`listdescriptors` `range` after each call: `[0, 999]` (`next_index` 1), `[0, 1000]` (2), `[0, 1001]` (3). That is
`next_index` + 998, one less than expected (`next + 999`). Cause, from the Core 32.1 source:
`DescriptorScriptPubKeyMan::GetNewDestination` calls `TopUp()` first (`src/wallet/scriptpubkeyman.cpp:948`), which sets
the exclusive range end to the old `next_index` + 1000 (`:1121`, keypool size 1000), and only then advances
`next_index`; `listdescriptors` prints the end minus one (`src/wallet/rpc/backup.cpp:543`). So the printed end is (new
`next_index` - 1) + 999 = `next_index` + 998. The test asserts that relation.

### Experiments not run, and the manual step

- Mempool eviction after `-mempoolexpiry` (336 hours by default) needs hours: not run.
- The 100-kill repetition of the index phase: done 3 times.
- The phone-wallet scan of the unified QR (`/state/onchain/unified.svg`, a `bitcoin:` URI with `amount` and `lightning`
  members): **not done**. It is a manual step; the test only proves the served QR is the QR of the URI built from the page's
  own fields and that the URI is within `qr.MaxInput`.
- `TestOnchainLate*`: the late payment is first seen after the expiry, so the forward carries code 4. The test shows it by the
  order state and events, not by reading the code.

### Limits of the setup

- The 300 s orders of `Partial` and `LateBefore` expire while later phases run; the safety check accepts their
  `order.underpaid` / `order.expired` only when a service ran past the expiry, requires them once one ran 15 s past it, and
  expects exactly the `ALERT`s of the events added in each run.
- Tier sizes are the defaults' (`up_to_sat` 1,000,000 and 10,000,000), configured explicitly since plan 6.1 with a 0 tier in
  front (`up_to_sat` 10,000, 0 confirmations); "tier 3" in `RestartBefore` is the tier of 3 confirmations (mined
  once, then 2 more while down).

### Plan 6.1 follow-ups

Every on-chain phase now runs on four tiers: `[{10,000, 0}, {1,000,000, 1}, {10,000,000, 3}, {0, 6}]`. The 5,000 sat orders
of `TestOnchainZeroConf` are paid from the mempool, and the orders of 100,000 sat and up keep their tiers. The ALERT check
counts the two lines of an `order.underpaid` together: at expiry, and at the end of the watch window. The paid-after-final
line is expected only where a phase causes it. The values below come from the final full run (`scripts/qualify.sh`, exit
0); the first run's values agree. A later run, after an on-chain order's `expired` and
`underpaid` came from its events and inheritance was limited to the window (decision 9), also exited 0; where its values
differ they are given below.

| Phase | Observed |
|---|---|
| `TestLightningAccountCap` | its own Lightning environment, the default cap of 5. Six Lightning-only orders of 21,000 sat for one fresh `account_id`: five presses give five invoices, and the sixth answers 303 to `?busy=1`, a page with "Busy, try again." and no invoice. An order of another fresh account gets an invoice. After the first invoice is paid, the sixth press gets an invoice. The safety check passes: one transfer, one `order.paid`, the other six orders neither paid nor expired. No ERROR or WARN line |
| `TestOnchainZeroConf` | A: `paid` from the mempool with no block (`confirmations` 0, `required_confirmations` 0, `confirmed_sat` 5,000), `order.paid` once, the page says Paid. Diverted by `bumpfee` with the `outputs` option: back to `new` / `waiting`, received 0, `order.reverted` seq 1 and the reversal `ALERT`; still `new` after the replacement is mined. A run before the hold of reversals on a 0 tier (the hold of the final event, extended to reversals) also met the split: there a `bumpfee` that kept the output landed between a pass's `listsinceblock` and its read of the old output, and B got `order.reverted` seq 1 then `order.paid_again` seq 1. Since that hold (the pass that marks a 0-tier record dead after its scan holds the reversal when it would turn the order unpaid), B: paid from the mempool, then `bumpfee` keeping the output: no `order.reverted` and no `order.paid_again`, one reversal `ALERT` for the old record (the log does not show whether this run's bump met the split: the held pass leaves no trace in the API or the webhooks). B ends `paid`, `confirmed_sat` 5,000 once, ledger net 5,000. Store: A has one output, dead; B has two, the replaced one dead and the replacement mined, in `onchain_settled` and not watched. Since plan 14 the phase also asserts that the node names the replacement at once: `gettxspendingprevout` for every input of B answers the replacement's txid, and that the watch wallet's `gettransaction` of the diverted payment A shows the diverting transaction in `mempoolconflicts` (polled until it does; it passed) |
| `TestOnchainWalletBehind` | a paid 0-confirmation order of 5,000 sat, with the wallet the rail reads held behind its node: its `gettransaction` and `listsinceblock` answers are frozen at their last live values (no block is mined while it lasts, and the test fails if one was) while the node's calls go through. W1: `bumpfee` keeping the output; `gettxspendingprevout` for the inputs of the original names the replacement; the log holds more `bitcoind's wallet has not yet processed a transaction in the node's mempool that replaces a payment` WARN lines after 5 polls; the order stays `paid`, received and confirmed 5,000 once, with no `order.reverted` and no `order.paid_again`; after the wallet is thawed and 6 blocks are mined it is still `paid` once. W2: a double spend to another wallet of a second paid order (`bumpfee` with the `outputs` option, no fallback used), the same checks with the wallet behind: the order stays `paid` for the 5 polls and no `order.reverted` is delivered; after the thaw it reads `new` / `waiting` with received 0, `order.reverted` seq 1 once, and stays so when the replacement is mined. Observed: divert to `new` 13.05 s (including the 5 polls held), the phase 55.9 s; the safety check over 11 orders: forward 14,263,378 sat, reversed 20,000 sat, events added `order.paid` 2 and `order.reverted` 1; the log held 10 of the WARN lines and 2 reorg `ALERT` lines (W2's reversal, and W1's old record reversed when the wallet showed the bump). Store: W1 has two outputs, the replaced one dead and the replacement mined, in `onchain_settled`; W2 has one, dead |
| `TestOnchainBumpAfterExpiry` | an order of 100,000 sat, `expires_in` 300, paid replaceable and seen before the expiry. Past the expiry it reads `new` / `seen` with no webhook for the order. `bumpfee` keeping the output: the replacement inherits the original's first-seen (298 s before the expiry) and is counted; `late_sat` stays 0; one block pays it, with one `order.paid`, no `order.expired`, no `order.late`, no ALERT. Store: the original dead and never recorded, the bump mined |
| `TestOnchainStuckBefore` / `After` | its own environment (`/state/onchain-stuck`, the same watch wallet and descriptor). Before: an order of 100,000 sat, `expires_in` 300, paid in full, left in the mempool. After: the service's clock (and the webhook receiver's) runs 7 days and 10 minutes ahead, past the watch window. The first pass reads `underpaid` / `underpaid` (received 100,000, 0 confirmations), with one `order.underpaid`, the window-end `ALERT`, and the page "not yet confirmed enough". One block: `paid`, 1 confirmation, `order.paid` and the paid-after-final `ALERT`. After 6 more blocks the output is in `onchain_settled` and nothing is watched |

MEASURED:
- Account cap: release to invoice 0.02 s (0.01 s in the later run). This is the time from the paid order reading `paid`
  to the sixth press getting an invoice; the first press after the payment already got one.
- 0-conf: send to `paid` 2.71 s (poll 2 s, no block); divert to `new` 1.77 s. In the later run: 2.72 s and 1.76 s.
- Bump after the expiry: block to `paid` 2.95 s; `bump_to_counted` 1.77 s (see the next paragraph). In the later run:
  2.75 s and 1.51 s.
- Stuck: window pass to `underpaid` 0.22 s from the service start (its first pass); block to `paid` 1.24 s. In the
  later run: 0.44 s and 1.27 s; `getmempoolentry` found the stuck payment before the start and after `order.underpaid`
  (now asserted).

OBSERVED, the split after a bump. The API was read every 100 ms for 8 s after the `bumpfee` past the expiry. It read
`expired` / `expired` 10 times (about 1 s), then `new` / `seen` for good: in the pass that marked the original dead, the
replacement was not yet listed. Both full runs met this split. The `onchain` change of plan 6.1 (no final event in the
pass that marks an on-time output dead) held the final event for that pass, so no `order.expired` was written. Without
it, that pass would have written `order.expired` for an order that was being paid. In those two runs the API state was
not held: it read `expired` for that one pass, with no event behind it. Since the API reads the final unpaid states from
the events, the later run met the split again and read `new` / `waiting` 9 times (about 1 s), then `new` / `seen` for
good, never `expired` (the phase now fails on an `expired` read).

OBSERVED, the replaced transaction while both are unconfirmed (Core 32.1, both the watch-only wallet and the keyed
merchant wallet):
- `gettransaction` of the replaced transaction reads `confirmations` 0 (not -1) until its replacement is mined, then -1
  (after 1 block) or -6 (after 6).
- Once the wallet has processed the replacement, the replaced transaction lists `walletconflicts` 1 and
  `mempoolconflicts` 1, and the replacement lists `walletconflicts` 1 (the replaced one). `listsinceblock` shows the
  same `walletconflicts` on both entries. So `walletconflicts` is filled while both are unconfirmed (observed in both
  wallets).
- Read right after `bumpfee`, the wallets may not know the replacement yet ("Invalid or non-wallet transaction id"),
  and the replaced one shows no conflict. This happened in both runs for the 0-conf bump.
- `getreceivedbyaddress` at `minconf` 0 counts both: 10,000 sat for the 5,000 sat order and 200,000 sat for the
  100,000 sat order, until the replacement is mined. Then it is 5,000 (or 100,000), and 0 for the diverted address. At
  `minconf` 1 it is 0 until the block. The safety check reads it at the order's depth after the phase's blocks, so for a 0
  tier it is a valid cross-check only once the replacement is mined. While a replacement is unconfirmed it overstates the
  receipt.

OBSERVED, the stuck payment across the restart: before the shifted service started and after its `order.underpaid`,
the miner wallet's `gettransaction` read 0 confirmations and `getmempoolentry` found the transaction. The payment
stayed in bitcoind's mempool across the service restart (bitcoind itself did not restart).

The restart bound of `TestOnchainRestartAfter` was tighter with the new phases' outputs: the final run had 11 watched
outputs (8 before plan 6.1), a first-pass bound of 4.02 s against 4.40 s for a second pass, and `paid` at 2.39 s. Since
then the phase bounds the catch-up by the second pass's settle plus the start time and one poll (6.63 s in the
later run) and logs whether the first pass did it: `paid` at 2.40 s, first-pass bound 4.03 s, on the first pass.

The first full run of the plan 6.1 phases failed in `TestOnchainPartial` on one unexpected
`ALERT: bitcoind call failed`. bitcoind's log showed no error. A service stop had landed while a pass was waiting on one
of its first calls, and the pass logged the cancelled call as a failure. That is fixed in `onchain` (plan 6.1: a stop
during a pass's first calls logs no failed call). An unexpected log line in the ALERT check now prints the method and
code of its records.

## Fiat pricing (plan 7)

Every rate provider in these phases is the fake rate server (`rates/ratestest`) inside the `qualify` process, reached
through the service's rate transport (`service.WithRateTransport`): the fake answers for the three built-in providers'
hosts and for `rates.example.invalid` (the custom rate API) and refuses every other host, so no live provider is called
on regtest. The rates block: Kraken, Coinbase, Yadio and the custom API, USD and EUR, `margin_percent` 1.5,
`min_sources` 2, `max_spread_percent` 2, `fetch_seconds` 10, `max_age_seconds` 60. The fake holds Kraken 76,000,
Coinbase 76,100, Yadio 75,950 and custom 76,050 USD (67,000, 67,100, 66,950 and 67,050 EUR). Each phase ends with the
safety check, and an ERROR or WARN line that the phase did not expect fails it. Measured in one full
`scripts/qualify.sh` run (every phase passed on the first run):

| Phase | Observed |
|---|---|
| `TestFiatPricing` | the first priced order (10.00 USD) was created 0.22 s after the service started (the first fetch runs at once), at rate 76025 (the median of the four) from four sources: 13,351,000 msat, `onchain_sat` 0; the page showed "10.00 USD" and the Yadio credit; paid over Lightning in 0.29 s; the `order.paid` body carried the same pricing (rate "76025", times in Unix seconds). An explicit order still worked (`pricing` null). Then every rate times 1.1 and a restart on the same data: the paid order still read rate 76025 and 13,351,000 msat, with the same pricing object; new orders priced at 83627.5 USD (12,138,000 msat) and 73727.5 EUR (13,767,000 msat), and the paid order, read again with the new rates live, was unchanged; `"rails":["onchain"]` answered 400 `onchain_disabled`. Every priced order's amounts re-derived from its own `pricing` (the audit) |
| `TestFiatOutage` | Kraken (500) and Coinbase (503) failed from the start: orders priced from Yadio and custom at 76000. Yadio then failed (502) right after a good answer: its last rate counted until it aged out. The probe (a price no rail can hold, 20 nines of USD, under a reference of its own: converted, so 400 `invalid_lightning_msat` while a rate counts, creating nothing) turned 503 `rate_unavailable` with `Retry-After: 10` **60.15 s** after that answer (the bound is `max_age_seconds` plus one interval, 70 s); the repeat of the first order's POST stayed 200 with that order throughout, the first order's reference with another price text ("10.000") was 409 `conflict` both while a rate counted and in the outage (looked up before any conversion, no `Retry-After`), and an explicit order was 201. One ALERT per currency, `reason` `too_few`, `providers` [custom] for USD and EUR. The custom rate moved to 90,000 while the other three failed: nothing new was logged. The failures cleared: the ALERT again, `reason` `disagree`, `providers` the four, `low` yadio, `high` custom, and the probe stayed 503; EUR logged `available`. The custom rate back at 76,050: a new order 201 from the four and USD's `available` line once. Then Yadio and custom served rates 10 minutes old and Kraken and Coinbase failed: the probe was 503 after 60.08 s (their cached rates counted until 60 s old), the repeat of the second order still 200, and the `too_few` ALERT with no providers. The state lines of the whole run: USD too_few, disagree, available, too_few; EUR too_few, available, too_few. `rate fetch failed` at WARN once per provider per run of failures (the later failures of a run are at DEBUG, outside the INFO log): Kraken 2 and Coinbase 2 (from the start, and again in the staleness step), Yadio 1, each with the provider, `class` status and exactly the HTTP status the fake gave it; 17 log lines in all; the fake's body marker (`ratestest.Sentinel`, in every failing answer) never reached the log |
| `TestOnchainFiat` | in the shared on-chain environment with the rates block added for this phase: 25.00 USD with the configured rails got `onchain_sat` 33,378 and `lightning_msat` 33,378,000 from one conversion at 76025 (1 confirmation tier); paid on-chain, `paid` 2.15 s after the payment was sent (1 block); `order.paid` carried the pricing; no ALERT |

The live smoke test (`go test -tags live -run '^TestLiveSmoke$' -v ./rates/`, by hand, one request to each built-in
provider) on 2026-11-06: Kraken 76,107 USD and 68,140 EUR, Coinbase 76,068 USD and 68,136 EUR, Yadio 76,128 USD and
68,323 EUR (Yadio's timestamp about 3 minutes behind the fetch, inside the default `max_age_seconds` of 300). The
spread was 7 basis points in USD and 27 in EUR (the default bound is 200), and the default rules would have priced an
order in both, at the medians 76,107 USD and 68,140 EUR.

## Monero end to end (plan 8)

The same service, with the Monero rail beside the on-chain one, runs in-process in the `qualify` container against a
regtest `monerod` and SatBooth's view-only `monero-wallet-rpc` (`wallet-satbooth`), both Monero v0.18.5.4 from the
official release tarball. `dev/regtest/monero/Dockerfile` pins the tarballs' SHA-256 from `hashes.txt`, whose PGP
signature (2026-10-16) was verified by hand with binaryFate's key read from `utils/gpg_keys/binaryfate.asc` at the tag,
fingerprint `81AC 591F E9C4 B65C 5806 AFC3 F0AF 4D46 2A0B DF92`. `monerod` runs `regtest`, `offline` and
`fixed-difficulty=1`; blocks come only from its `generateblocks` RPC. A second wallet-rpc without a login
(`wallet-customer`) holds the test's two full wallets: `customer` pays and receives every mined block, and `merchant`
is the merchant's own wallet, which confirms receipts. `TestMoneroSetup` writes the merchant's address and view key (no
spend key) to `/state/monero/view.json` in the state volume. `wallet-satbooth`'s entrypoint generates the view-only
wallet from it once (`monero-wallet-cli --generate-from-json`), and a later start reopens the existing file. It then
starts wallet-rpc with the Digest login from `RPC_LOGIN` in its environment, never in argv. The view key and the wallet
password exist only in the state volume.

All Monero phases share one environment (`/state/monero`, one data directory), except the stuck pair
(`/state/monero-stuck`, a clock 7 days and 10 minutes ahead), as the on-chain phases do. The configuration: `network`
`regtest`, `address` the merchant's primary address, tiers of 1, 3 and 10 confirmations at 1 XMR, 10 XMR and above
(`TestMoneroPoolDrop`'s second half: the first tier at 0), `poll_seconds` 2, and the on-chain rail of the on-chain phases
beside it. Every phase ends with the on-chain phases' safety check: events against the expected range per type, seqs
contiguous, `order.paid` at most once, one successful delivery each, and the on-chain side of a mixed order. The Monero
side follows: for each payment, the forward and reversal transfers of every epoch in ledger 3 are paired, with code 2 for
a counted payment and 4 for a late one. The net per subaddress equals what the merchant's own full wallet receives there
(`get_transfers` on `merchant`, at the order's depth, the pool too for a 0 tier), and the ledger accounts equal the sums.
Every record at ERROR or WARN that a phase does not expect fails it, `wallet-rpc listed entries SatBooth could not read`
included: since ruling 8-T10a-2 the watcher skips entries for addresses SatBooth never issued (the primary address, and
subaddresses not in its store, such as the setup's two and each environment's payments for the other) without that line,
and counts a payment listed both mined and in the pool once. The harness asks `wallet-satbooth` itself (through `xmrrpc`,
with the Digest login) only while no service runs. That is caution, not a known limit: a second Digest client beside
the service was not tried, and epee keeps the Digest state (nonce and counter) per connection (`http_server_auth m_auth`
in `http_custom_handler`, `contrib/epee/include/net/http_protocol_handler.h:190` and `:227`, the per-connection
`m_handler` of `abstract_tcp_server2.h:271`, at v0.18.5.4).

Values from the full run (`scripts/qualify.sh`, exit 0, every phase passed on the first run). After a later change
(ruling 8-T10a-2, the creating-nothing probe in `TestMoneroFiat`, the graceful stop around the wallet-file copy, the
condition-based reorg wait) the Monero section of `scripts/qualify.sh` ran again on a fresh regtest project: all 16
phases passed with no `wallet-rpc listed entries SatBooth could not read` line in any phase (99 in `TestMoneroPay`
before); where its values differ they are given below.

| Phase | Observed |
|---|---|
| `TestMoneroSetup` | 120 blocks to the customer (height 121); `wallet-satbooth` answered `get_version` with the Digest login 16.0 s after the phase began polling it (the view-only wallet's description written and the blocks mined just before), already at the daemon's height; the inferred facts below; one transaction paying subaddresses 1 and 2 of `merchant`; the service's startup `Check` passed against the real wallet-rpc. Observed: `TestMoneroKillBefore`'s three orders got indices 15 to 17 (logged). Derived from those: the twelve Monero orders pressed before it (Pay 3, Partial 3, PoolDrop 2, Reorg 2, UnlockTime 1, RestartBefore 1, one subaddress each) took indices 3 to 14, so the view-only wallet counted the two paid indices as used and SatBooth's first subaddress was index 3 (not logged itself) |
| `TestMoneroPay` | three orders, one per tier (0.5 XMR / 1 confirmation, 5 XMR / 3, 20 XMR / 10). The page of a Monero-only order offers exactly the "Pay with Monero" form (`rail=monero`); the press shows the subaddress and "Send exactly … XMR."; the served `qr.svg?rail=monero` is byte for byte `qr.SVG(MoneroURI(subaddress, amount))` (114 to 116-byte URI, limit 1,200, saved as `/state/monero/monero.svg`) and the page's "Open in wallet" link is that URI. Paid from `customer`; blocks mined one at a time with the API read after each: "Seen, 0 of N confirmations", then `confirmations` N in the API until the tier block, `paid` at the tier block and never before; `order.paid` once each |
| `TestMoneroPartial` | 60 % then 40 % (each mined once): `seen` with `confirmed_piconero` 0.3 XMR and "0.3 XMR seen so far." on the page, then `paid`, `confirmed_piconero` the sum, not overpaid; 120 %: `paid`, `overpaid`, one overpaid `ALERT`, the refund line on the page |
| `TestMoneroPoolDrop` | a payment seen in the pool and then `flush_txpool`ed before a block: the order back to `new` / `waiting` with nothing received 9.0 s after the flush, the page "Waiting for payment", nothing recorded after a block; at its expiry (300 s) one `order.expired`. Then the 0-conf configuration (the service restarted): `paid` from the pool with no block (`confirmations` 0), flushed: `new` / `waiting` 13.3 s later, `order.reverted` and the reversal `ALERT`; `relay_tx` of the transfer's saved `tx_metadata` was accepted (same txid) and one block gave `order.paid_again` and its `ALERT` |
| `TestMoneroReorg` | see the reorg section |
| `TestMoneroUnlockTime` | a payment locked by its sender could not be made: see "Differences from expectations". The `transfer` with `unlock_time` tip + 1,000 was refused with -50 and nothing was sent; the order stayed `new` / `waiting` with nothing received or late |
| `TestMoneroRestartBefore` / `After` | an order of the 3-confirmation tier paid and mined once; the service stops; 2 blocks mined while it is down; the phase waits until `wallet-satbooth` has them (8.0 s: it refreshes every 20 s); the new process reads it `paid` with 3 confirmations 1.49 s after the launch (start 0.24 s), on its first pass (the second runs 2 s after the start); one forward transfer, one `order.paid` |
| `TestMoneroKillBefore` / `After` | the script stops `wallet-satbooth` gracefully (exit 0: wallet-rpc stores the wallet on the signal, `wallet_rpc_server.cpp:310-323` and `:5011-5029`; an earlier run copied while it ran), copies its wallet files aside and starts it again; three orders are pressed (indices 15, 16, 17); `wallet-satbooth` is killed (SIGKILL, exit 137), the copy restored over the files it stored since, and it starts again. The restored wallet answers `get_address` of index 17 with -15. The service's startup `Check` re-created the three (one re-creation `ALERT`, `count` 3) and passed; afterwards `get_address` of index 17 equals the stored subaddress (the inferred determinism, observed); three new orders got 18, 19 and 20; a payment to the first re-created subaddress was found and paid the order (15.1 s after its block) |
| `TestMoneroLateBefore` / `After` | an order with `expires_in` 300, nothing paid; once expired it reads `expired`, one `order.expired`; a payment after the expiry reads `late` (`late_piconero` from first sight in the pool), never `paid`, and `order.late` comes only once it is mined (none during the quiet period before the block), with one late `ALERT`. The 60 % order of `Partial` (also 300 s) reads `underpaid` (one `order.underpaid`, one `ALERT`, during `PoolDrop`) and after a 40 % top-up `late` with `late_piconero` 0.2 XMR and one `order.late` |
| `TestMoneroStuckBefore` / `After` | its own environment (`/state/monero-stuck`, the same wallet). Before: 0.5 XMR, `expires_in` 300, paid in full and left in the pool. After: the clock 7 days and 10 minutes ahead, the payment still in `monerod`'s pool; the first pass reads `underpaid` (received 0.5 XMR, 0 confirmations) 0.42 s after the start, one `order.underpaid`, the window-end `ALERT`, the page "not yet confirmed enough". One block: `paid`, 1 confirmation, `order.paid` and the paid-after-final `ALERT`. After 10 more blocks the payment is settled and nothing is watched |
| `TestMoneroFiat` | the fake rate server: BTC from Kraken, Coinbase and Bitfinex; XMR/USD from Bitfinex (250) and Kraken (251); XMR/EUR from Kraken alone. 25.00 USD with `rails` `["onchain","monero"]`: `monero_piconero` 101,297,405,190 = `Piconero(25.00, 250.5, 1.5 %)`, `onchain_sat` 33,367 at 76,050, `pricing.rates` with `BTC` and `XMR`. The same in EUR: 400 `invalid_currency`, no `Retry-After`, and no fiat `ALERT` naming XMR EUR in the whole log; `["onchain"]` in EUR: 201. Bitfinex failing (500): the probe of `TestFiatOutage` (20 nines of USD on the Monero rail alone, which creates nothing) answered 400 `invalid_monero_piconero` while Bitfinex's cached rate counted, then 503 `rate_unavailable` with `Retry-After: 10` 60.3 s after the failure began (60.2 s in a later run, after 61 probes; an earlier run still polled with real orders and kept 30 of them unpaid), and one `ALERT` naming XMR USD (`too_few`); `rate fetch failed` at WARN once for Bitfinex BTC and once for Bitfinex XMR (one run of failures per provider and asset), class `status`, status 500, no provider body in the log. Cleared: the recovery line for XMR USD. The first order pressed with `rail=monero`, paid, mined once: `paid` 4.8 s after the block, `order.paid` with the same pricing |
| `TestMoneroBothRails` | an order of 100,000 sat on-chain and 0.5 XMR: the page offers both rails; both pressed (address and subaddress); paid on-chain and mined: `paid`, `order.paid`; then paid in Monero and mined: the Monero payment recorded (6.2 s after the block), the order still `paid`, `order.paid` delivered once, the refund line on the page, and the both-rails `ALERT` twice, once per rail (each rail logs it on its record when the other holds money) |

### MEASURED

Paid latency per tier (poll interval 2 s; `wallet-rpc` refreshes its wallet from `monerod` every 20 s,
`DEFAULT_AUTO_REFRESH_PERIOD` at `src/wallet/wallet_rpc_server.cpp:64`, while `get_transfers` and `get_transfer_by_txid`
read the pool on every call, `:2966` and `:3049`):

| Tier | send to `seen` | tier block to `paid` |
|---|---|---|
| 1 confirmation | 1.17 s | 6.54 s |
| 3 confirmations | 1.09 s | 20.62 s |
| 10 confirmations | 1.01 s | 20.60 s |

A pool payment shows within one poll; a block within one auto-refresh of `wallet-satbooth` plus one poll, so a block is
seen up to about 22 s after it is mined, depending on where it falls in the refresh period (the tier-1 block: 6.5 s). Other timings: second part mined
to `paid` 20.0 s; overpaid block to `paid` 19.7 s; 0-conf send to `paid` 1.86 s; relay and block to `paid_again` 19.2 s;
pop and flush to `new` 17.5 s; late top-up block to `late` 18.9 s (the full late payment already read `late` in the pool,
so its 0.07 s is not a block-to-late time); the stuck payment's block to `paid` 17.2 s; the priced order's block to `paid`
4.8 s; the Monero QR SVGs 6.4 KB.

### The facts inferred from the source, read on the real wallet-rpc

| Fact | Observed | Pinned in the code |
|---|---|---|
| wallet-rpc version | `get_version` 1.32 (major 1, minor 32) | `Check` step 1 requires major 1 |
| `sign_transfer` with an empty `unsigned_txset` | view-only wallet -29 (`WALLET_RPC_ERROR_CODE_WATCH_ONLY`); full wallet (`merchant`) -39 (`WALLET_RPC_ERROR_CODE_BAD_UNSIGNED_TX_DATA`, "cannot load unsigned_txset", `wallet_rpc_server.cpp:1403`) | -29 alone passes (`codeWatchOnly`): agrees |
| `get_transfer_by_txid` of an unknown txid | -8 | `codeTxNotFound` -8: agrees |
| `get_address{address_index:[1000]}` on a fresh wallet | -15 | `codeIndexRange` -15: agrees |
| a wallet call with no wallet open | -13 (`get_height` after `close_wallet`, on `wallet-customer`) | `codeNotOpen` -13: agrees |
| the 401's header lines | status line, `Server: Epee-based`, `Content-Length: 98`, `Content-Type: text/html`, `Last-Modified`, `Accept-Ranges: bytes`, and two `WWW-authenticate` lines (`qop="auth"`, `algorithm=MD5`, then `algorithm=MD5-sess`, `realm="monero-rpc"`, `stale=false`) | equal to `xmrrpc/testdata/challenge.txt` with the nonce and the date masked: agrees |
| one transaction paying two subaddresses | `get_transfer_by_txid` lists both in `transfers` (indices 1 and 2) | `GetTransferByTxID` reads `transfers`: agrees |
| re-creation after a lost wallet file | `create_address` from the restored wallet gives the same subaddress at the same index | `Check` step 5 re-creates and compares: agrees |
| 20 concurrent `get_height` through one `xmrrpc.Client` | all 20 succeeded | the client sends one request at a time; the mutex stays. Concurrent clients against the server were not tried (the harness never runs a second Digest client beside the service; epee's Digest state is per connection) |

No pinned code and no line of `challenge.txt` differed, so no correction commit was needed.

### Reorg (`pop_blocks`)

`pop_blocks` is not a JSON-RPC method at v0.18.5.4: it is the URI `/pop_blocks` (`src/rpc/core_rpc_server.h:142`).

- Plain: pay, mine 1 (`paid`), stop the service, `pop_blocks` 1 (monerod from height 139 to 138). The harness then read
  `wallet-satbooth`'s height every second for 45 s (two auto-refresh periods and a poll; 30 s, one reading, in an
  earlier run). Before any new block the transaction was back in `monerod`'s pool, yet `wallet-satbooth` stayed at
  height 139 throughout, with `get_transfer_by_txid` showing it `in` at height 138. The merchant's wallet after an
  explicit `refresh` listed it twice, `in` at height 138 and `pool`. Neither wallet drops the popped block until a new
  block arrives, so the restarted service still read `paid` with 1 confirmation (not stale), and no height `ALERT` came.
  After 2 blocks: `paid` with 2 confirmations 6.1 s later. The popped transaction was re-mined in the first new block,
  with no event (the safety check accepts none, or `order.reverted` and `order.paid_again` once each).
- Dropped: pay, mine 1 (`paid`), `pop_blocks` 1, `flush_txpool` of the payment, mine 2: `new` / `waiting` 17.5 s later,
  `order.reverted` seq 1 and the reversal `ALERT`.

### wallet-rpc's login from a config file (plan 8)

A throwaway container of the same image (no network, removed afterwards) ran `monero-wallet-rpc --config-file <file>`
with a 0600 file holding `rpc-login=satbooth:<a random test password>` and `non-interactive=1`, no `--rpc-login`, no
`RPC_LOGIN` and no wallet open (`--wallet-dir`). `get_version` without a login answered 401, with the file's login
through Digest 200 (a result), and with a wrong password 401; wallet-rpc's argv held no `rpc-login`. The wallet-rpc
example in `docs/monero.md` uses this route.

### Differences from expectations

- **A payment locked by its sender cannot be made at v0.18.5.4.** wallet-rpc's `transfer` refuses a non-zero
  `unlock_time` with -50 (`WALLET_RPC_ERROR_CODE_NONZERO_UNLOCK_TIME`, "Transaction cannot have non-zero unlock time",
  `src/wallet/wallet_rpc_server.cpp:1266-1271`, and the same in the other transfer handlers). `monerod`'s pool refuses
  such a transaction from relay (`src/cryptonote_core/tx_pool.cpp:185-191`, `tvc.m_nonzero_unlock_time`), while a block
  may still carry one, so only a miner can pay with a lock. `TestMoneroUnlockTime` therefore asserts wallet-rpc's refusal
  (a later release that allows it again fails there) and that nothing reached the order. The pool's refusal is cited
  from the source, not asserted: no such transaction can be built through wallet-rpc. The late-by-lock rule, its code-4
  transfer and its two `ALERT`s are covered by `monerotest`, not by this run.
- **`--generate-from-json` takes the new wallet's password from the JSON.** `wallet2::make_from_json` gives every generate
  call the JSON's `password` member (empty when absent) and reads no `--password-file` for it. So `view.json` carries the
  same password as `wallet.pass`, which wallet-rpc's `--password-file` opens the wallet with.
- **`monerod` refuses `non-interactive` in a config file** ("Option 'non-interactive' is not allowed in the config file"):
  it is on the command line in `compose.yml`, and `monerod.conf` holds the other keys.
- **The foreign-entries WARN repeated on every pass (fixed, ruling 8-T10a-2).** In an earlier run, `wallet-rpc
  listed entries SatBooth could not read` was logged once per pass for as long as `get_transfers`' window (the cursor
  less the deepest tier, about 11 blocks) held a payment to a subaddress the store did not issue: in `TestMoneroPay`, 99
  lines for the setup's two-subaddress payment. Since the fix the watcher skips entries for addresses SatBooth never
  issued (the primary address, subaddresses not in its store) without the line, counts a (txid, subaddress) listed both
  `in` and `pool` once (the mined entry wins), and keeps the line for entries that fail the shape checks or repeat a key
  within one list. The Monero phases allow zero such lines.

### Experiments not run, and the manual step

- `double_spend_seen` is not reproducible on regtest without crafting a conflicting transaction, which needs the
  customer wallet's key images outside wallet-rpc's API: covered by `monerotest`.
- A payment locked by its sender: refused at v0.18.5.4 (above); covered by `monerotest`.
- The pause of a running watcher while `Check` fails (ruling 8-T7c) is not exercised on regtest: no phase replaces the
  wallet under a running service. `KillAfter` is the restore path: a wallet behind on subaddresses is not paused.
- Concurrent Digest clients against one wallet-rpc: not tried (see the Digest rows above).
- The phone-wallet scan of the Monero QR (`/state/monero/monero.svg`, a `monero:` URI with `tx_amount` only): **not
  done**. It is a manual step; the run only proves the served QR is the QR of the URI built from the page's own fields
  and that the URI is within `qr.MaxInput`.

## Lightning refunds (plan 9)

The whole service runs in-process as in the Lightning phases, now with the `payout` block: the bootstrap's payout
connection (paired with `pay_invoice`, `lookup_invoice` and `get_info`, the 50,000 sat lifetime budget, renewal `never`)
through its own relay pair on `relay-satbooth`, and an approval key generated in the test from a fresh random seed
(approvals signed in-process with `approval.SignRefund` and `approval.SignVoid`). The merchant's hub plays the customer
(it pays the order) and the claimant's wallet (its invoice is the one the refund pays). Every phase ends with checks on
the API (refund view and order figures), on both hubs' own rows for the refund's payment hash, on the payout budget
(`get_budget`, read on a connection of the test's own while no service runs) and, after the service stopped, on the
ledger's three refund transfers (pending, post, void). Each phase stops the service with a list of the `ALERT` lines
it allows, each for named refund ids only: in the shared environment, "the hub has no record of a sent refund" for
the crash and cut refunds (from their phases on, until `TestRefundVoidUnknown` voids them); in `TestRefundBudget`,
the budget line for each of its two refunds, once each. Any other `ALERT` line fails the phase.
All phases but `TestRefundBudget` share one environment (`/state/lightning/refund`).

| Phase | Observed |
|---|---|
| `TestRefundPay` | first, an invoice of 1,001 sat from the merchant's hub claimed under a 1,000 sat approval: 400 `invalid_invoice` at claim; over the next sender round the payout connection had 0 lookups and 0 `pay_invoice` calls (counted), and neither hub has a row for that invoice. Then a 1,000 sat refund of a 2,000 sat order: claim answered 202 at once (0.00 s), `settled` 23.6 s after the claim (the resolver's 30 s round). SatBooth's hub: one outgoing `settled` row of 1,000,000 msat, fee 0; the merchant's hub: one incoming `settled` row of 1,000,000 msat. Order figures `paid_msat` 2,000,000, `refunded_msat` 0 then 1,000,000, `refund_open_msat` 0. Budget used +1,000,000 msat (amount + fee 0). Ledger: pending and post. The same approval again: 409 `refund_exists`; a new approval with the same invoice: 400 `invalid_invoice` (one refund per invoice) |
| `TestRefundPartials` | 800 and 1,200 sat refunds of a 2,000 sat order, claimed back to back, both `settled` 29.8 and 30.8 s after the claims; a third of 1 sat: 409 `exceeds_refundable` while the two were open and again after they settled; figures `refunded_msat` 2,000,000; one settled row of each amount on each hub |
| `TestRefundOwnInvoice` | the order's own invoice (issued by SatBooth's store, paid by the customer): 400 `own_invoice`, nothing stored |
| `TestRefundHubKnown` | a 100 sat merchant invoice paid directly on the payout connection first (fee 0), then claimed: `failed` `invoice_known` 0.3 s after the claim (the sender's lookup found the outgoing row). No new row at either hub, budget unchanged, no ledger transfer |
| `TestRefundCrashAfterMarker` | a child SatBooth process (the test binary run again, the service started as `cmd/satbooth` starts it) whose payout connection's `pay_invoice` sends SIGKILL to its own process: killed 0.04 s after the claim, between the send marker and the request (the sender calls `pay_invoice` right after it stores the marker). The data after the kill: send marker, ledger pending, no result; no row at either hub. Restarted in-process with the payout calls counted: the `ALERT` "the hub has no record of a sent refund" 30.3 s after the start (the resolver keeps a no-record answer silent for 15 s after the send marker, so the first round after the restart was silent and the next one alerted), the refund `sent`, `refund_open_msat` 1,000,000, **0 `pay_invoice` calls** over the start-up and one more resolver round (35 s), still no row at either hub |
| `TestRefundCut` | relay-satbooth takes the in-process proxy's URL (as for `TestLightningRelayCut`), and both of the service's connections, invoice and payout, reach it through the proxy; the payout connection's `pay_invoice` cuts the proxy first (every connection closed, new ones refused), then the real client sends: it returned after 15.0 s with "no response from the wallet; the request may or may not have been carried out: context deadline exceeded" (the 15 s hub call bound), the refund `sent`. Proxy restored: the `ALERT` (no record) 16.0 s later. A void at that point: 409 `refund_in_flight` (the 11 minute wait). No row at either hub, ledger pending only |
| `TestRefundFailedBefore` / `Failed` / `FailedAfter` | the merchant's hub (the claimant's node and SatBooth's only channel peer) stopped by the script (`docker compose stop -t 60`) before the claim of a 2,000 sat refund: the hub wrote one outgoing row and reported it `failed`, failure reason "NodeError: PaymentSendingFailed: Failed to send the given payment."; the refund read `failed` (`failed`) 32.1 s after the claim (the resolver's round). `refund_open_msat` 0, the ledger pending voided, budget unchanged (10,100,000 msat used before and after). With the merchant's hub back (relay recreated, channel active), a new approval for the whole 2,000 sat was admitted (the failed refund freed it) and paid a fresh invoice: `settled` 32.0 s after the claim, one settled row of 2,000,000 msat on each hub, `refunded_msat` 2,000,000, budget +2,000,000 msat; the failed payout's row stayed one `failed` row, and the merchant's hub has no row for its invoice |
| `TestRefundBudget` | its own environment; two claims over the budget left, one voided while `claimed` (plan 11). Measured in a targeted run (`scripts/qualify.sh --only refund`, the setup and the refund group alone, so 5,100,000 msat of the payout budget were used, by the earlier refund phases only). Budget left 44,900,000 msat; two orders of 44,901,000 msat paid, then two claims of all of each, 1 sat more than the budget left, the first with an approval valid 150 s, the second valid an hour: the `ALERT` "the hub budget left is less than the refund and its fee headroom" for each 0.3 s after the claims, both refunds `claimed`, `refund_open_msat` 44,901,000 on each order. A void of the second while it was `claimed`: 200 in 0.00 s, `failed` `cancelled` with `resolved_at`; its order back to `refund_open_msat` 0; a second void: 409 `invalid_state`; no row at either hub for its invoice. The first ended `failed` `expired_unsent`, 154.5 s after the claims. Two `ALERT` lines, one per refund; no send marker for either; the ledger of the first has no transfer (pending, post, void all absent) and the second's pending exists and is voided, not posted; budget 5,100,000 msat used before and after |
| `TestRefundVoidUnknown` | the crash and cut refunds, 354 s and 284 s after their send at the phase's start: a void then: 409 `refund_in_flight`; the phase waited the remaining 377 s of the 11 minute void wait, then each void answered 200 `failed` `voided` with `resolved_at`; a second void: 409 `invalid_state`. Both orders back to `refund_open_msat` 0 (refundable again), no row at either hub, the ledger pending of each voided |

Payout budget across the suite: 7,000,000 msat used when the refund phases start (QualifyTest's 3,000 sat payout and
the two crash phases' 2,000 sat; the cancelled holds and the failed payout free theirs), 12,100,000 msat after them
(1,000 + 2,000 + 100 + 2,000 sat paid, all fees 0 on the direct channel), so 37,900,000 msat stay.

The targeted run `scripts/qualify.sh --only refund` (plan 11) took 14.8 minutes (888 s, images cached), every refund
phase passing: `TestRefundBudget` 155 s, `TestRefundVoidUnknown` 373 s of waiting (the crash and cut refunds 359 s and
289 s after their send at its start; an early void 409 `refund_in_flight`, then both 200 `failed` `voided`). The other
phases' figures matched the full run's above within a second (`TestRefundPay` settled 23.6 s after the claim, the
partials 30.7 s, `TestRefundFailed` 32.1 s, `TestRefundFailedAfter` 31.9 s).

Time: the refund section takes about 13.5 minutes (809 s of phase time in the latest full run):
`TestRefundBudget` waits 150 s for its approval to expire and `TestRefundVoidUnknown` waits out the 11 minute void wait
of the cut refund (377 s; the phases in between hide the rest). The whole `scripts/qualify.sh`, every plan's phases,
took 82.6 minutes (images built and cached), within the qualify job's 120 minute limit.

What these phases show, and what they do not:
- A hub-reported `failed` is observed, for a payment the node could not route (its only peer offline). The case the
  design guards with a new approval, a payment the hub marks `failed` because it is shut down mid-payment and that
  settles after the restart, is still not observed (Payouts in flight above); it is documented for operators.
- "Never reached the hub" is produced two ways: a real process kill between the marker and the request (no seam in
  `refund`: the payout connection is wrapped through `service.WithPayoutWallet`, which the tests already use), and
  a relay connection cut right before the request. In both the hub has no record, SatBooth raises one `ALERT` per
  process, and only a person's void ends the refund.
- The pending `ALERT` after an hour, `key_revoked`, `not_sent` (a relay refusal) and recovery from the ledger are
  not exercised live; the refund package's crash and state-machine tests cover them with the fake wallet
  (`refund/refundtest`).

## The late Lightning payment (plan 10)

The question: can a payment sent before a SatBooth invoice's expiry settle at SatBooth's hub after SatBooth's closing
lookup has read it unsettled, so that the order reads `paid` after `order.expired` with the late `ALERT`? **By source,
the receiving side accepts such a late settle. It is not reproduced live**: the only way to hold the payment on
regtest, a paused routing hub, does not hold it deterministically (below). The late path stays covered with the fake
wallet (lnpay `TestLateSettle*`).

Pinned revisions, read as raw files:
- Alby Hub v1.24.6 = getAlby/hub `921003b65ece004551612647ce1df22327a2486d`;
- its `ldk-node-go` `v0.0.0-20261021094837-02f88de84f63` (hub `go.mod:16`), built from getAlby/ldk-node
  `87ea53f2e1f32cc7487521776ae10f9e3850a891` (ldk-node 0.7.1). *Inferred* from CI timing: the bindings commit does not
  name its source commit;
- rust-lightning (`lightning`) 0.2.7, tag v0.2.7, `5ce2ecb33d32e3b61ef01d5ead0ee6693c0cd7d1`, from the fork's
  `Cargo.lock`, with the `std` feature on. That the crates.io release equals the tag is *assumed*.

What the receiver does, *source*:
1. **rust-lightning compares an inbound payment's expiry with `highest_seen_timestamp`, the highest block header time
   it has seen, never the wall clock.** The receive path calls `inbound_payment::verify(…,
   self.highest_seen_timestamp…)` (`lightning/src/ln/channelmanager.rs:7929-7933`), which fails the HTLC only if
   `expiry < highest_seen_timestamp` (`lightning/src/ln/inbound_payment.rs:442-445`). The stored expiry is
   `highest_seen_timestamp + invoice_expiry_delta_secs + 7200` (`inbound_payment.rs:238-247`). The value moves only when
   a block connects (`best_block_updated`, `channelmanager.rs:14317-14333`), and a reload restores the stored value. So
   with no block mined a late HTLC passes, and with blocks at wall time it passes up to about two hours past the
   invoice's expiry. The final hop's CLTV check is by height (`channelmanager.rs:7944-7950`), and the claim has no time
   check.
2. **ldk-node claims it by itself.** Its `PaymentClaimable` handler refuses only unknown even TLVs, a circular or
   outbound record, a duplicate or already-succeeded payment and an LSP fee over the limit, and otherwise calls
   `claim_funds(preimage)` (`src/event.rs:639-958`, the claim at `:925`); it has no expiry test.
3. **The hub records the settle with no expiry check.** `nwc_lnclient_payment_received` looks up the incoming row by
   payment hash with no state filter (`transactions/transactions_service.go:812-815`) and calls
   `markTransactionSettled` (`:872`), which refuses only an empty preimage (`:1480-1483`), then publishes
   `nwc_payment_received`, the NWC `payment_received` notification. Nothing marks an expired incoming invoice failed.
   While unsettled past its expiry, `lookup_invoice` shows the invoice as `expired`
   (`nip47/models/transactions.go:28-31`, display only); once settled, `settled`. So SatBooth's closing lookup reads the invoice unsettled, and a later lookup
   or the notification reads it settled.

Why it is not reproduced on regtest: the paying hub refuses to pay an expired invoice ("Lightning end to end" above),
so a live late settle needs a payment sent in time and held on its way across the expiry. Regtest has no
deterministic hold. A third hub as a router, frozen with `docker pause`, is not a neutral hold: its peers send a ping
every 10 s and disconnect a peer that does not answer (`lightning/src/ln/peer_handler.rs:3541-` and `:3579-3584`;
`lightning-background-processor/src/lib.rs:128-129`), and on resume the router fails the HTLC back
(`ChannelDisabled`, `ChannelNotReady`, `PeerOffline`; `channelmanager.rs:4951-4956`,
`lightning/src/ln/channel.rs:12637-12647`) unless the HTLC was already in the router's channel to SatBooth's hub when it froze. A pause long enough to cross an
expiry and the closing lookup therefore succeeds or fails by timing, and a phase that passes by a race would make the
suite flaky.

## The release images (plan 10)

Qualification runs the shipped images, as a hybrid (plan 10). The relays always run from the release
image, and a new phase group drives a `satbooth` container over HTTP only. The phases that need in-process hooks keep
running `service.Run` in the qualify container, built with the image's flags.

### What ran where

| Where | Phases | Why |
|---|---|---|
| `satbooth-relay` release image (`deploy/image/Dockerfile`, target `satbooth-relay`) | `relay-satbooth` and `relay-merchant` in every phase | Nothing in the suite needs a hook in the relay. The old regtest-only relay image is gone. |
| `satbooth` release image (target `satbooth`), over HTTP only | `TestImageStart`, `TestImageLightningPay`, `TestImageOnchainPay`, `TestImageMoneroPay`, `TestImageRefund`, `TestImageSecondContainerRefused`, `TestImageRestartBefore`/`After`, `TestImageBackupBefore` (once) and `TestImageBackupAfter` (twice, after each restore of a matched pair). `TestImagePrepare` runs before the container is first created, and `TestImageBudget` after its last stop. | These cover the start-up checks, the three rails, a refund, the lock, a restart, and backup and restore. Each needs only what a merchant, a customer and an operator see: the API, the checkout page, the webhooks, the exit codes and the log lines. `qualify/image_test.go` imports nothing from `service` or `store`. It takes the webhook type `order.paid` from `docs/webhooks.md` as a literal, and checks the event id's documented shape (32 lowercase hex characters). Its other SatBooth imports do not define what the image must do: `webhook.Verify` is the reference verifier for the documented signature, `approval` is the operator's signing format, `btcrpc.FormatBTC` formats the customer's amount for bitcoind, and `nwc` reads the payout budget from the hub. |
| In-process `service.Run` in the qualify container | every other phase: Lightning, fiat, on-chain, Monero, refunds | They need hooks the image does not have: an injected clock (expiry, the 7-day stuck pairs), the fake rate server, the wrapped payout wallet (the kill between the send marker and the request, the relay cut), the wallet-rpc kill-and-restore, and store and ledger reads after a stop. |
| — | no fiat phase on the image | The rate providers need `https`, and the closed regtest network has no internet. The in-process fiat phases (`TestFiatPricing`, `TestFiatOutage`, `TestOnchainFiat`, `TestMoneroFiat`) run the same code against the fake rate server. |

The image group runs last, after `TestRefundVoidUnknown`, and alone. It uses the same invoice and payout connections,
the same watch-only wallet (`satbooth-watch`) and the same view-only wallet as the in-process phases. Its data
directory is fresh, so the watch wallet's `next_index` and the view-only wallet's subaddresses are ahead of its record,
as for the stuck environments.

**Build-flag parity.** The image builds with `GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0` and
`go build -trimpath -ldflags=-buildid=`. The qualify container's environment now sets `GOFLAGS=-mod=vendor -trimpath`
beside the other three. `-ldflags=-buildid=` is left out because it changes only the build id, not the code.
`TestBuildFlags` runs first and checks the qualify test binary:
- from `debug.ReadBuildInfo`: `-trimpath=true`, `CGO_ENABLED=0` and `GOOS=linux`. Both flags are recorded build settings;
- for vendor mode: `-mod=vendor` is not a recorded setting, which a probe confirmed. So the phase reads it from
  `GOFLAGS` and from the dependencies. A module-mode build records each dependency's `go.sum` hash and a vendor build
  records none (seen on the two builds of `cmd/satbooth`). The phase requires that none of the 10 dependencies has a
  hash.

Measured: `go1.27.4 -trimpath=true CGO_ENABLED=0 GOOS=linux GOARCH=arm64 deps=10 (none with a go.sum hash)
GOFLAGS="-mod=vendor -trimpath"`.

### The Compose services and their options

Every option has a reason. The run options are the ones `deploy/README.md` gives for production.

| Service | Option or mount | Reason |
|---|---|---|
| `relay-satbooth`, `relay-merchant` | build `deploy/image/Dockerfile` target `satbooth-relay` | The relays are the release image. |
| | `read_only`, `cap_drop: [ALL]`, `no-new-privileges` | How the image ships (`deploy/README.md`). |
| | `command: ["-config", "/state/relay-<hub>.json"]` | The release image's ENTRYPOINT is the bare binary. The old image baked a config path into it. |
| | `state:/state:ro` | The relay's config, written by `TestBootstrap` with mode 0644: public data. In regtest the relays run as the same uid as SatBooth (65532) and mount the whole state volume, so they could read SatBooth's secret files. In production they run on the other host, the hub's. |
| `satbooth` | build target `satbooth`, `user: "65532:65532"` | The release image and its numeric user. |
| | `read_only: true`, `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]` | How the image ships. |
| | `ulimits: memlock: -1` | `mlockall` needs the rlimit, not a capability. Every start logged "memory locked; core dumps disabled". |
| | `stop_grace_period: 60s` | Compose's default of 10 s ends in SIGKILL, while the worst-case shutdown with an `alerts` block is 40 s: the shutdown grace (10 s), the alert drain's last flush (10 s) and each database `Close` (up to 10 s). Every stop exited 0 by itself. |
| | `172.30.78.40`, `public_listen` `172.30.78.40:9101`, `private_listen` `172.30.78.40:9102` | The listeners must be specific IPs (`service/config.go`). Both are on the container's fixed address so that the qualify container reaches the API. Ports above 1024, because the container has no capabilities. |
| | `state:/state:ro` | The configuration and secret files in `/state/image` (directory 0700, files 0600, owner 65532), as an operator bind-mounts secret files. |
| | `satbooth-data:/data` | `data_dir`, prepared by `volume-init`. |
| | no `restart` policy, no `depends_on` the hubs | `qualify.sh` reads the exit codes of a refused start. The hubs and relays already run when the group starts. |
| | `depends_on: volume-init: condition: service_completed_successfully` | The volumes must belong to 65532 before the first start. Compose runs `volume-init` again on every `start` of `satbooth`, as the run log shows. That is harmless: it does the same `chown` and `chmod` of three directories. |
| `satbooth-oneoff` | the same image and hardening, `network_mode: none` | A second process on the same data volume, and `satbooth backup`. Both act before any connection: the lock is taken before any network call, and a backup needs none. A fixed IP would collide with the running `satbooth`. |
| | `state:/state:ro`, `satbooth-data:/data`, `satbooth-backups:/backups` | The same configuration. The data volume is read-write because the backup takes the flock. `/backups` is the writable place for `-to`. |
| `volume-init` | the pinned builder image (`golang:1.27.4-trixie@sha256:b54b2639...`), `user: "0:0"`, `cap_drop: [ALL]`, `cap_add: [CHOWN, FOWNER]`, `no-new-privileges`, `network_mode: none` | Docker creates named volumes owned by root, and `scratch` has no `chown`. It runs `chown 65532:65532` and `chmod 0700` on `/data`, `/backups` and `/state/image`, and needs only these two capabilities. |
| | run again by `qualify.sh` as `--user 65532:65532` with `--entrypoint sh` | The restore helper. The copied files stay the image user's. |
| `qualify` | `GOFLAGS: -mod=vendor -trimpath` | Build-flag parity (above). |

### Where `qualify.sh` acts between the phases

The qualify container has no Docker socket. Every stop, start and one-off run is done by `scripts/qualify.sh`, as for
`RelayLoss*`. Every call passes `--profile manual --profile image`, so that `down -v` and the failure logs cover the
image's containers and volumes. The checks read the container's exit codes and its own log lines. Those are fixed
messages without values. After each step the script compares the counts of "satbooth started", "satbooth stopped",
"memory locked" and "startup failed" with what it expects, and requires no `ALERT`, "ended unexpectedly" or "did not close
cleanly" line.

1. `TestImagePrepare` writes the configuration and the secrets and reads the payout budget. Then `docker compose up -d
   satbooth` runs `volume-init` first. `TestImageStart` follows, then the log check.
2. After the rails and the refund, a second container: `satbooth-oneoff -config …` on the same data volume exited 1
   with `startup failed` (`data_dir_lock`) and "another SatBooth process holds the data directory". The script checks
   the exit code, the `startup failed` line, the `data_dir_lock` reason and the lock message, and that the output has no `ALERT` or "did not close
   cleanly" line. Then `TestImageSecondContainerRefused`.
3. `TestImageRestartBefore`. Then `stop -t 60` of the running container, which must exit 0 (every stop checks first that the container runs), and `start`. Then `TestImageRestartAfter`.
4. `TestImageBackupBefore`. Then the script stops the container (exit 0) and runs `satbooth backup -config … -to
   /backups/b1` in `satbooth-oneoff`, which must exit 0 with "backup written" and print no `ALERT` or "did not close
   cleanly" line. Next comes the restore of `docs/backup-and-restore.md`. It checks each file's SHA-256 against the
   entry of its backup's `manifest.json` that names the file (so a hash cannot match the other file's entry), removes `store.db*` and `ledger.db*` (the `-wal` and `-shm` files with them) and copies
   both files. It then checks the live files' SHA-256 against the manifests again, so every round proves that the data
   directory holds the backup's bytes. Then it starts the container, and `TestImageBackupAfter` runs.
5. The mixed pair: stop, back up to `b2` (one generation later), then restore the store from `b1` and the ledger from
   `b2`, and start. The container exited 1, and the line "the store is older than the ledger: restore both databases
   from one backup" rose by exactly one. Then the remedy: `b2`'s own pair is restored and started, and
   `TestImageBackupAfter` runs again.
6. Stop (exit 0). Then the public bundle's phases ("Deployment bundles" below), then `TestImageBudget`.

### Phases

| Phase | Asserts | Observed |
|---|---|---|
| `TestImagePrepare` | It writes `satbooth-image.json` from plain JSON, as an operator writes it, with every rail but fiat and the `payout` block, a `webhooks` list of two endpoints (`main` at `/hook` and `second` at `/hook2` of the same receiver, each with a secret file of its own), and seven secret files, each 0600 and owned by 65532. The two webhook secrets differ. It reads the payout budget before the container runs. | files `api-keys.json nwc.key payout.key rpc.pass satbooth-image.json webhook.secret webhook2.secret xmr-rpc.pass` |
| `TestImageStart` | The container serves, so every start-up check passed: the invoice and payout connections' methods at the real hub, bitcoind's and wallet-rpc's checks, and the pair stamp. SatBooth opens its listeners only after these. The read key answers 404 for an unknown order and a stranger's key gets 401. Each rail takes an order (on-chain and Monero at 1 confirmation). | listeners up before the phase's first poll |
| `TestImageLightningPay` | It opens an invoice and the merchant's hub pays it. There is one settled row on each hub, the order reads `paid`, one `order.paid` (seq 1) is delivered to each of the two endpoints and verified, and the page says Paid. The receiver of each endpoint verifies with that endpoint's own secret and answers 400 to any other signature, so a delivery it counted was signed with its own secret. Every phase that waits for `order.paid` (`payLN` and the on-chain phase) makes the same check. | paid 0.39 s after the payment; both endpoints received the event once |
| `TestImageOnchainPay` | 100,000 sat to the issued address, the same on the page and in the API: `seen`, then one block, then `paid`, then `order.paid`. The merchant's keyed wallet received exactly the amount at the address. | paid 2.07 s after the block (poll 2 s) |
| `TestImageMoneroPay` | 0.5 XMR to the issued subaddress: `seen`, then one block, then `paid`, then `order.paid`. The merchant's full wallet received exactly the amount there. | paid 17.79 s after the block (wallet-rpc refreshes every 20 s) |
| `TestImageRefund` | A 2,000 sat order is paid, then a claim of 1,000 sat to a merchant invoice. It answers 202 `claimed`, then reads `settled`. SatBooth's hub has one outgoing settled row of 1,000,000 msat and the merchant's hub one incoming row. The order's figures read `refunded_msat` 1,000,000 and `refund_open_msat` 0. | settled 13.8 s after the claim, fee 0 |
| `TestImageSecondContainerRefused` | After the second container's refusal, the first still serves: the paid order reads `paid`, and a new order is taken. | — |
| `TestImageRestartBefore` / `After` | An invoice is opened, then the container stops cleanly and starts again. The same invoice is still on the page, it is paid, and the order reads `paid` with one `order.paid`. | stop 0 s, exit 0; paid 0.41 s after the payment |
| `TestImageBackupBefore` / `After` | One paid and two open orders are made, and the state of all five image orders made so far is recorded. After each restore of one backup's pair, every one reads the same and a new order is taken. | backups at generations 2 and 3; mixed pair refused, exit 1 |
| `TestImageBudget` | With the container stopped, the payout budget moved by exactly the refund and its fee. The harness file's refund amount must equal the 1,000 sat constant. | 12,100,000 then 13,100,000 msat used of 50,000,000 (+1,000,000: the refund, fee 0) |

Payout budget at the end of the suite: 13,100,000 msat used of 50,000,000. That is the 12,100,000 after the refund
phases plus the image's 1,000 sat refund.

Time: the whole `scripts/qualify.sh`, every plan's phases, the image group and the public bundle's phases, took 84.0
minutes (5,037 s, images built and cached; 87 test passes), within the qualify job's 120 minute limit. The image
group's 13 phases took 56.5 s of phase time and the public bundle's 3 phases 1.6 s, plus the container stops, starts and
one-off runs between them (the bundle's whole step, under a minute).

### Limits

- **Two endpoints in the image only.** The release image's phases run with two webhook endpoints (above); the other
  groups and the in-process phases run with one, `main`. Which events a new, a removed, a re-added and a renamed
  endpoint is sent, and the abandoned line with its endpoint, are covered by `service`'s tests with real HTTP
  receivers, not by the regtest. The deploy group's bundle has the one endpoint `main` that the setup command writes.
- **No forced spill inside the image.** No API path sorts at least 20,000 rows: the API reads one order or one refund by
  id, and there is no list route. So the temp_store pragma is verified by `TestTempStoreNoTempDir`
  (`store/temp_test.go`), not forced inside the image. `VACUUM INTO` in the
  backup does not spill, so it is no substitute.
- **Restarts in-process before plan 10.** A restart there had a window in which SQLite could still hold a file after
  `Run` returned. Since plan 10, `Close` returns only after SQLite has closed the file, which closed it. The
  image's restarts are real process exits, so the window did not apply to them.
- **No fiat on the image** (above). The image's `https` path, its CA bundle, is not exercised by qualify.

## Deployment bundles (plan 10; plan 12)

`scripts/qualify.sh --only deploy` runs the setup (the build, `TestBuildFlags`, the hubs and `TestBootstrap`, the relays
and the hub restart) and then this group alone, for a targeted run; it is not the qualification, which runs the group
inside the full suite. The group starts both bundles from what `deploy/setup.sh` (the tools image with
`satbooth-setup`, run with no network) writes, not from files the test writes: the public host's bundle,
`deploy/public/compose.yml`, from stages 1 and 4, then the hub host's bundle, `deploy/hub/compose.yml`, from stages 2
and 3 (first start). Both compose files run unmodified.

### The public bundle, from stages 1 and 4

`scripts/qualify.sh` runs it as a project of its own, `satbooth-regtest-deploy`, with two regtest files:

- `dev/regtest/deploy-public.env` in place of the generated `compose.env`: `PUBLIC_ADDR` `127.0.0.1`, `PUBLIC_PORT`
  19101, and the bundle's network on `192.0.2.0/24` with SatBooth at `192.0.2.40`, because the regtest network already
  holds `172.30.78.0/24` (the generated file's default network). `SATBOOTH_ETC` and `SATBOOTH_BACKUPS` are the
  directories of stage 1, passed from `qualify.sh`'s environment. The generated `compose.env` is checked instead: it
  exists, is mode 0600, and `docker compose -f deploy/public/compose.yml --env-file <it> config -q` passes.
- `dev/regtest/deploy-public.override.yml`, the one change: SatBooth also joins the regtest network (internal) as
  `172.30.78.41`, where the relay, the webhook receiver and the phases are. The bundle's own network, the public
  listener on it and the one published port stay as shipped.

The phases run in `qualify-deploy` (`dev/regtest/compose.yml`): the qualify container, also on the bundle's network as
`192.0.2.30`. They talk HTTP only. The configuration is Lightning with refunds on (the answers have an approval public key, a published test vector, and
the four caps; no descriptor, no Monero, no alerts and no currencies), so the bundle starts with the generated `payout`
block and its start-up check of the payout connection passes. It makes no refund: `TestDeployPublicNoPayout` reads the
bundle's payout connection from the hub and requires its budget to be the answers' 1,000,000 sat with 0 used. The group
runs after the image group's last stop and before `TestImageBudget`; the bundle's connections are their own apps in the
regtest hub, so the suite's `payout` connection, whose budget `TestImageBudget` reads, is untouched.

1. `deploy/setup.sh <dir> public init -answers <file>`: `network` regtest, `hub_private_addr` 198.51.100.20,
   `public_private_addr` 198.51.100.10 (documentation addresses: the regtest hub side lives in `172.30.78.0/24`, which
   the command refuses as a private-link address), `checkout_base_url` `http://192.0.2.40:8080`, `webhook_url`
   `http://172.30.78.30:9100/hook` (the regtest receiver; the command accepts an `http` URL), `approval_keys` (the public
   key of RFC 8032's first test vector), `payout_budget_sat` 1,000,000, `payout_max_sat` 100,000, `payout_daily_sat`
   500,000, `payout_alert_sat` 0. The answers file is a temporary file, removed afterwards. The digest the stage printed equals `shasum -a 256` of `setup/public-to-hub.json`.
2. `qualify.sh` copies the handoff (public), the two plain order API keys (`setup/api-keys/orders-read.key`,
   `orders-write.key`; stage 1 also writes the two refund keys, which the group does not use) and `etc/webhook.secret` into `/state/deploy` as root (mode 0600), without printing them.
3. `TestDeployPublicConnect` reads the handoff and creates both connections in the regtest hub for the two client keys
   stage 1 generated: the invoice connection with the scopes of the invoice link (`make_invoice`, `lookup_invoice`,
   `list_transactions`, `notifications`; no budget) and the payout connection with the scopes and budget of the payout
   link (`pay_invoice`, `lookup_invoice`, 1,000,000 sat, renewal `never`). It adds both pairs to `relay-satbooth.json`
   and writes `hub-to-public.json` (canonical, both Wallet Public Keys) and its digest. `qualify.sh` recreates the relay
   to load the pairs.
4. `deploy/setup.sh <dir> public finish -handoff <file> -digest <digest>`.
5. `qualify.sh` checks that `etc/` holds exactly `api-keys.json nwc-client.key payout.key satbooth.json webhook.secret`,
   each mode 0600. It then asserts, with `jq -e`, the values stage 1 and stage 4 generated for the keys it patches (the
   table's "Generated" column; the listen addresses come from the generated `compose.env`'s `SATBOOTH_IP`), and that the
   configuration has the key `webhooks` with exactly one entry, named `main`, whose URL is the answer's (and not the
   old key `webhook`), that the checkout URL is the answer's, and that there is no `onchain` or `alerts` block. Then it
   patches `etc/satbooth.json` (`jq`) and checks that the file with those four keys deleted is the same as before. The
   patched keys, and nothing else:

   | Key | Generated (asserted) | Patched to |
   |---|---|---|
   | `public_listen` | `<SATBOOTH_IP>:8080` (`172.30.78.40` by default) | `192.0.2.40:8080` (`SATBOOTH_IP` of `deploy-public.env`) |
   | `private_listen` | `<SATBOOTH_IP>:8081` | `172.30.78.41:8081` (the override's address on the regtest network) |
   | `nwc.relay_url` | `ws://198.51.100.20:7448` | `ws://172.30.78.10:7448` (the regtest relay; its `client_relay_url` is the same) |
   | `payout.relay_url` | `ws://198.51.100.20:7448` | `ws://172.30.78.10:7448` (the same relay serves both connections) |

   No key, secret or API key hash of the running bundle comes from the test. `TestDeployPublicPrepare` only reads the
   plain order API keys and the webhook secret that stage 1 wrote, for the phases that call the API.
6. `docker compose up -d satbooth` (the bundle's `volume-init` first), then `TestDeployPublicStart`.
7. `qualify.sh` reads the container from the engine and its log (table below).
8. `TestDeployPublicPay`, the restart count again, a graceful stop and the log again.
9. `satbooth backup` in the bundle's `satbooth-oneoff`, as `deploy/README.md` "Backups" runs it, into
   `SATBOOTH_BACKUPS`: exit 0, "backup written" (generation 1), no `ALERT` or "did not close cleanly" line.
10. A start, `TestDeployPublicAfterBackup` (one order that the backup does not hold), a graceful stop.
11. The restore of `deploy/README.md` "Restore", with its commands: `stop`, `run volume-init`, then as 65532 without a
    network in `volume-init`'s image the manifest check of both files of `d1`, `rm -f /data/store.db* /data/ledger.db*`,
    the copy and the check of the copies; then `start`, `TestDeployPublicRestored`, `TestDeployPublicNoPayout` and a
    graceful stop.
12. `down -v` of the project and removal of the directories with a root helper (they belong to root and 65532 by then).

### The hub bundle, from stages 1 to 3

A second project, `satbooth-regtest-deploy-hub`, from `deploy/hub/compose.yml` and the `compose.env` of stage 2. Its
Docker networks are `172.30.79.0/25` (`hub_subnet`, the hub at `172.30.79.10`) and `172.30.79.128/25`
(`electrum_subnet`), apart from the regtest network (`172.30.78.0/24`) and the public bundle's (`192.0.2.0/24`).

1. A stage 1 run of its own (a scratch directory) with `hub_private_addr` 127.0.0.1 and `public_private_addr` 127.0.0.2
   (loopback is accepted on regtest only): Lightning, the on-chain rail (a regtest `wpkh` descriptor) and a refund
   approval key (the public key of RFC 8032's first test vector, with the four caps), so the handoff carries the payout
   client key and SatBooth's `rpcauth` line. Its printed digest equals the file's SHA-256.
2. A stage 2 run with `hub_admin_addr` 127.0.0.3, a different `hub_private_addr` (127.0.0.9) in the answers: exit 1,
   "the two hosts were given different values: hub_private_addr", and the directory still empty.
3. Stage 2 with the right answers: `compose.env` mode 0600, no `relay.json`.
4. The published ports: `HUB_PRIVATE_ADDR` is 127.0.0.1 (7448, 8332). The generated `compose.env` carries
   `HUB_ADMIN_ADDR=127.0.0.3` (asserted): the setup command requires the admin address to differ from the two
   private-link addresses. On Linux it is used as written and the admin port is asserted to be published on
   `127.0.0.3:8080`. On macOS, Docker Desktop binds 127.0.0.1 alone (`bind: can't assign requested address` for any
   other 127.x address), so `qualify.sh` gives Compose `HUB_ADMIN_ADDR=127.0.0.1` in its environment, over the env file.
   Before `up`, 127.0.0.1:7448, :8332 and :8080 are probed; a taken one is not touched, and
   `dev/regtest/deploy-hub-ports.override.yml` remaps the three published ports to free ones for the run. The file is
   read by `docker compose config -q` on every run.
5. `up -d --build hub`: `volume-init` exits 0 (no `relay.json` yet), `netns`, bitcoind and the hub run; bitcoind mounts
   the generated `bitcoin.conf` and reports `chain` `regtest`.
   After 8 s the hub container itself is running with a restart count of 0 and its HTTP port (`GET /api/info` on the
   published admin address) answers: it needs neither its relay nor Fulcrum to be there.
6. RPC over the published port (the passwords go to `curl` through a pipe, never in argv or output):

   | User, password | Call | HTTP |
   |---|---|---|
   | `hub`, from `config/hub.env` (read by a root helper) | `getblockchaininfo` | 200 |
   | `hub` | `getblockcount` | 403 |
   | `satbooth`, from `etc/bitcoind.pass` of stage 1 | `getblockchaininfo` | 200 |
   | `satbooth` | `getblockcount` | 403 |
   | `satbooth`, a wrong password | `getblockchaininfo` | 401 |

7. Stage 3 with two made-up valid Wallet Public Keys (distinct, not the client keys), then `up -d relay`: it keeps
   running (restart count 0, after 8 s) and `volume-init` exits 0 again, with a later `FinishedAt` than after the first start; `relay.json` is owned by 65532 (Docker Desktop
   presents ownership as seen by the reader, as below).
8. After the relay's start the hub is checked again (running, restart count 0, HTTP port answering). Exactly `bitcoind
   hub netns relay` are running, `volume-init` has exited 0 and no other container exists: Fulcrum and the Monero profile are
   not created. `down -v` and the
   removal of the directories follow, also on failure.

| Read by `qualify.sh` | Required and observed |
|---|---|
| `HostConfig.ReadonlyRootfs` | `true` |
| `HostConfig.CapDrop`, `CapAdd` | `["ALL"]`, `null` |
| `HostConfig.SecurityOpt` | `["no-new-privileges:true"]` |
| `HostConfig.Ulimits` | `memlock=-1/-1` |
| `Config.User` | `65532:65532` |
| `HostConfig.RestartPolicy.Name`, `RestartCount` | `unless-stopped`, 0 after the start and after the payment (a refused start would have looped instead of exiting) |
| `HostConfig.PortBindings` | exactly `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"19101"}]}` |
| `docker port` | exactly `8080/tcp -> 127.0.0.1:19101` |
| `GET http://127.0.0.1:19101/static/style.css` from the host | 200 |
| log lines | 1 "satbooth started", 1 "memory locked; core dumps disabled", no "startup failed", `ALERT`, "ended unexpectedly" or "did not close cleanly"; after each stop, one "satbooth stopped" per start (3 in all) |
| exit code of `stop -t 60` | 0 |

| Phase | Asserts | Observed |
|---|---|---|
| `TestDeployPublicConnect` | The invoice and payout connections for stage 1's two client keys, their pairs in the relay, and the canonical `hub-to-public.json` with its digest. | connections made, handoff written |
| `TestDeployPublicNoPayout` | The bundle, started with the generated `payout` block, made no refund: its payout connection has the 1,000,000 sat budget with 0 used. | budget 1,000,000, used 0 |
| `TestDeployPublicPrepare` | Reads the plain API keys and the webhook secret stage 1 wrote (the test writes no file of the bundle). | 3 values read |
| `TestDeployPublicStart` | The container serves, so every start-up check (the payout connection's included) passed against the real hub (it opens its listeners only after them). The read key answers 404 for an unknown order, a stranger's key 401, and a Lightning order opens its checkout page on the public listener. | listeners up before the phase's first poll |
| `TestDeployPublicPay` | One Lightning order paid end to end: the merchant's hub pays, one settled row on each hub, the order reads `paid`, one signed `order.paid` (seq 1) is delivered, and the page says Paid. | paid 0.54 s after the payment |
| `TestDeployPublicAfterBackup` | One order, made after the backup `d1`. | created |
| `TestDeployPublicRestored` | After the restore of `d1`: the paid order reads `paid`, the order made after the backup answers 404, and a new order is taken. | passed (`--only deploy`) |

The payout budget read by `TestImageBudget` after the bundle's step: 12,100,000 msat before the image group, 13,100,000
after (the image's 1,000 sat refund, fee 0), the same as without the bundle's step.

### Secrets on bind mounts

Proved first on a throwaway container (Docker Desktop on macOS, Engine 29.9.2), with dummy files: SatBooth's loaders
accepted a bind-mounted directory of 0600 files read as 65532 (the start went on to the relay dial), and refused the
same files at 0644 (`configuration refused`, `nwc.client_key_file`). Docker Desktop presents a shared file as owned by
whichever user reads it (stat in the container showed `65532:65532` for files the host user owns), keeping the host
mode; a `chown` inside the container is recorded too. On Linux a bind mount keeps the host's numeric owner. The
bundles' `volume-init` therefore `chown`s the configuration directory to its reader and never changes a file's mode,
which works on both. The qualify run copies the files in root-owned, but on Docker Desktop this cannot show that the
`chown` is what makes them readable; on Linux it is.

### The hub bundle, the rest of it, run once by hand

The first start is qualify's (above); what follows is not. With a scratch env file (`HUB_NETWORK=regtest`, a scratch subnet, `HUB_PRIVATE_ADDR` `127.0.0.1`)
and scratch regtest configurations:

- `up -d --build`: `volume-init` exited 0, then `netns`, bitcoind, the relay and the hub ran. The hub, set up and
  started through its API on the published port, reached `running: true` with bitcoind as its chain source
  (`rpc_host` `bitcoind`, port 8332, user `hub`, the password only in the bind-mounted `hub.env`), and connected to the
  relay's hub listener on loopback. The relay's client listener answered 426 on the published 7448.
- Restarting the relay alone: the hub reconnected to it. Restarting the hub alone: the relay kept answering, and the
  hub (locked, as there is no `AUTO_UNLOCK_PASSWORD`) rejoined the namespace and reconnected once started again.
- `--profile monero` without the Monero files: monerod's start stopped with "bind source path does not exist", and
  no path was created on the host. With the files: a wallet made with the bundle's `monero-wallet-cli` through `run
  --rm --no-deps --entrypoint monero-wallet-cli wallet-rpc` (a new throwaway wallet, not `docs/monero.md`'s
  `--generate-from-view-key`, which asks for the keys interactively); wallet-rpc answered `get_version` on the published
  18083 with 200 with the Digest login and 401 without, and its argv held no login.
- `exec bitcoind bitcoin-cli -conf=/etc/bitcoin/bitcoin.conf -datadir=/data getblockcount` worked with the cookie.
- With Fulcrum in the bundle: Fulcrum indexed bitcoind through its whitelisted user and served; its log had no 403 or
  method-not-found line; as `fulcrum`, `getblockchaininfo` answered 200 and `getwalletinfo` and `stop` 403. The hub's
  address lookup (`GET /api/wallet/address`) reached Fulcrum on the `electrum` network with no lookup error.
- The networks: from the hub's namespace, bitcoind and Fulcrum answered, and monerod's name did not resolve nor its
  address answer; from Fulcrum, nothing outside the `electrum` network was reachable. From Fulcrum the hub's API on
  8080 did answer, and the relay's client listener did not (threat model §5, the accepted residual).
- monerod with `--restricted-rpc`: `set_bootstrap_daemon` answered 404 and `get_info` read `restricted: true`; a
  regtest wallet-rpc refreshed through a restricted port and listed its incoming transfers (a throwaway wallet-rpc
  with the regtest-only `--allow-mismatched-daemon-version`, since the bundle's own refuses a regtest daemon).
- `scripts/volume-init-test.sh` ran each bundle's `volume-init` twice; the second run needs `DAC_READ_SEARCH` to list
  the directories it gave away in the first. On Docker Desktop owners are masked, so its run in CI on Linux
  (`.github/workflows/image.yml`) is the one that shows the owners.
- Before that, alone: the hub as uid 10003 with a read-only root, no capabilities and `.env` from a bind-mounted 0600
  file in its working directory (a `PORT` set only there took effect); both node images built; the wallet-rpc
  entrypoint refused a missing wallet, a 0644 login and a login without `user:`.

Not run: a reboot of the daemon with the bundle up (the ordering of `netns` before the hub and the relay is read in the
moby source, `deploy/README.md` "Restarts"), mainnet or signet sync, and a real view-only wallet.

### Limits

- The bundle's merchant API is reached on the bundle's network in qualify; a merchant application on another host has
  no way in by design of the bundle (`deploy/README.md`, "The merchant API").
- The runs recorded here were on Docker Desktop on macOS, where a bind mount masks file owners (see "Secrets on bind
  mounts"). qualify also runs on Linux, the ship platform, in CI (`.github/workflows/qualify.yml`, on the runner named by
  the repository variable `RUNNER`, GitHub's hosted runner when it is unset, weekly and by hand), where a bind mount
  keeps real owners.


## The hubs' bitcoind user (plan 11)

Both regtest hubs reach bitcoind as the user `hub`, confined by `rpcwhitelist` to the eleven calls of their chain
source (`dev/regtest/bitcoind/bitcoin.conf`, the same line as `deploy/hub/config-example/bitcoin.conf`; the source
lines are in `docs/dependencies.md` §8). The mining and funding helpers keep the unrestricted `regtest` user
(`rpcwhitelistdefault=0`). Bitcoin Core 32.1 refuses a call outside a user's list with HTTP 403 and logs, at warning
level and so with no debug category, `RPC User hub not allowed to call method <name>` (`src/httprpc.cpp:117` at
v32.1). `scripts/qualify.sh` ends every mode (the full run, `--only deploy`, `--only image`, `--only refund`, `--only
hub`, `--only onchain`) with a check: each regtest hub container runs with `LDK_BITCOIND_RPC_USER=hub` (its environment
as the engine applied it, from the refund run on); then no such line for `hub` in the regtest bitcoind's log over the
run; then one `getblockcount` as `hub` through the qualify container must answer 403 and add exactly one line, which
proves the matcher. The public bundle runs no
bitcoind of its own, so the regtest one is the only one checked.

Measured in the targeted runs:

| Run | Refused calls for `hub` | The probe |
|---|---|---|
| `--only deploy` (72 s) | 0 | 403, one line added |
| `--only refund` (888 s) | 0 | 403, one line added |
| `--only image` (198 s) | 0 | 403, one line added |

What ran on the confined user, in these runs: each hub's start and sync, the bootstrap (the merchant's on-chain
funding seen from the mempool and in blocks, the 1,000,000 sat channel's funding transaction broadcast by the merchant's
hub and confirmed over 6 blocks), every payment over that channel (the top-up, the orders, the refunds, the payout the
hub reports `failed`), hub restarts and a hub stopped and started (`TestRefundFailed`), and, with `--only image`, the
on-chain and Monero set-up phases and the release image's phases. The chain source asks for fee estimates when the
best block changes (ldk-node `src/chain/bitcoind.rs:310-327`), and once, blocking, at the node's start (`src/lib.rs:248`).
On regtest a failed estimate falls back to 1 sat/vB without stopping anything (`bitcoind.rs:538-547`; on mainnet it
fails the update, :533-536, and at start it stops the start), so on regtest a refused
`estimatesmartfee` or `getmempoolinfo` would show only as a log line: the end check is what catches it.

What runs in no phase of the suite, on any user: a cooperative or force close of a channel, a sweep of a closed
channel's outputs, and a fee bump (anchor or RBF) of the hub's own transactions. The bitcoind kills under the hubs
(`TestOnchainIndexBefore` to `TestOnchainIndexAfter`) run only in the full suite, not in these targeted runs. The
gossip checks (`getblockhash`, `gettxout`) are likely not exercised: the regtest channel is private and the two hubs
have no other peers, so no channel announcement reaches either hub. Those paths use only methods in the list
(`docs/dependencies.md` §8: every broadcast is `sendrawtransaction`), but that is read in the source, not observed.
