# SatBooth design

SatBooth was built in stages called plans, numbered 1 to 14, with a plan 6.1 for the follow-ups of plan 6. The sections
of §3 titled "... as built (plan N)" describe what plans 5 to 14 built; plans 1 to 4 are described in the earlier
sections. The numbered items of such a section are its decisions, cited as "decision N" (for example plan 6.1, decision
8). The lettered or labelled items made while building are rulings, cited as "Ruling X" or by the plan's number and a
label (for example Ruling D or 6.1-W). Both words are used as labels in the other documents and in code comments.

Plans 1 to 14 and the follow-ups of plan 6.1 are built: the Nostr core, the ledger, the relay, the NWC client, the hub
qualification suite, the Lightning payment service (§3, "Lightning payments, as built"), on-chain bitcoin (§3,
"On-chain payments, as built", and "On-chain follow-ups, as built (plan 6.1)"), fiat pricing (§3, "Fiat pricing, as
built (plan 7)"), Monero, view-only (§3, "Monero payments, as built (plan 8)"), Lightning refunds (§3, "Lightning
refunds, as built (plan 9)"), operations (§3, "Operations, as built (plan 10)"), the void of any open refund (§3, "The
void, as built (plan 11)"), the setup command (§3, "The setup command, as built (plan 12)"), several webhook endpoints
(§3, "Several webhook endpoints, as built (plan 13)") and the final fixes (§3, "The final fixes, as built (plan 14)").
Nothing has run on mainnet. The facts below were read from the named upstream sources on the dates given. Check them
again before relying on them, because upstream code moves.

## 1. Scope (v1)

- **Invoices:** create a payment request for an order on one or more rails, with an expiry. As built, **the merchant's
  app sends either the amount for each rail in that rail's base units** (msat, sat, piconero) **or a price in a fiat
  currency**. SatBooth first fetched no exchange rates and did no currency math; since plan 7 it converts a price in a
  fiat currency into per-rail amounts (plan 7, built: the median of configured providers, a margin, the rate locked and
  recorded with the order); explicit per-rail amounts still work. Report the status: `new`, `paid`, `expired`,
  `underpaid` and `late` (see §2.2); overpaid is a flag on a paid order. After an expiry, the app decides whether a
  retry keeps the price or gets a new quote.
- **Checkout page:** a minimal hosted page (QR, amount, countdown, status by polling SatBooth, never the
  node).
- **Notifications to the merchant's app:** signed webhooks (HMAC with timestamp). A webhook is only a
  hint: the app re-reads the status from SatBooth.
- **Refunds go back on the rail the payment came in on**: Lightning to Lightning,
  bitcoin to bitcoin, Monero to Monero. Only Lightning refunds are paid by SatBooth, capped and
  human-approved (threat model §4), and only for orders paid over Lightning, in msat. A person pays
  on-chain and Monero refunds from the merchant's wallet.
- **Approval format and signer:** this repo defines the approval format (Ed25519 over a fixed byte
  layout, with test vectors) and ships a small Go signing tool. Any admin system can produce the same
  format instead.
- **Operations (plan 10):** release images, an offline backup of the two databases as a matched pair with a stamp that
  keeps them from drifting apart, and an operator webhook for `ALERT:` lines (§3, "Operations, as built (plan 10)").
- **Not in v1:** on-chain or Monero refunds by the server (a human pays them from the merchant's wallet),
  multi-tenant hosting, card payments (keep the existing Stripe integration on the merchant side), a
  plugin system, a theme engine.

**Status of §1 (plan 10; plan 9; plan 8; plan 7; plan 6 with the plan 6.1 follow-ups).** Built for Lightning, on-chain bitcoin and
Monero: invoices, addresses and subaddresses, the checkout page with one unified bitcoin QR and a Monero QR behind a
rail picker, and signed webhooks. Since plan 6.1 a confirmation tier may be 0
(paid from the mempool), an on-time payment still unconfirmed at the end of its 7-day window reads `underpaid` and
stays tracked, and each `account_id` may hold at most `max_open_per_account` open Lightning invoices. An order has a Lightning amount, an on-chain amount, or both. Its states are
`new`, `paid`, `expired`, `underpaid` and `late`, derived from both rails (`status`); the on-chain rail also reports
its own state, with `overpaid` among them (`docs/merchant-api.md`, "The merchant API"). The webhook events are
`order.paid`, `order.expired`, `order.underpaid`, `order.late`, `order.reverted` and `order.paid_again`. Since plan 7 an
order may instead carry a price in a fiat currency (with the `rates` block): SatBooth converts it with the median of the
configured providers and the operator's margin into the same number of sats on each rail, and records the rate with the
order (§3, "Fiat pricing, as built (plan 7)"). Since plan 8 an order may also carry a Monero amount (`monero_piconero`),
paid to a fresh subaddress of the merchant's view-only wallet; its states and events are the same, derived from all
three rails, and a priced order gets Monero only when its `rails` name it, from its own XMR quote (§3, "Monero payments,
as built (plan 8)"). Since plan 9 SatBooth pays Lightning refunds of orders paid over Lightning, each approved by a
person with a signed approval, capped per order, per refund and per day, and bounded by the payout connection's lifetime
budget at the hub (§3, "Lightning refunds, as built (plan 9)"). On-chain and Monero refunds are paid by a person from
the merchant's wallet; SatBooth does not record them yet. Since plan 10 each host's deployment bundle is in `deploy/`
(§3, "Operations, as built (plan 10)").

**Deployment requirement:** the Alby Hub (and its data directory, which holds the NWC secrets) runs on a
different host from the public-facing SatBooth service. If both share a host, compromising one compromises
both, and the "bounded loss" in the threat model no longer holds.

## 2. Rails

### 2.1 Lightning: a dedicated Alby Hub over NWC

The Lightning side is a **dedicated** Alby Hub (github.com/getAlby/hub, LDK backend), used only by
SatBooth. It is never the merchant's personal wallet. It gets one channel, opened from the merchant's
main node or an LSP, kept small, and swept back to the main node regularly by paying an invoice from that
node. **The operator sweeps** from the hub's own console, which only they can reach. SatBooth has no
connection that can sweep; it only alerts when the balance goes over the sweep threshold (threat model §9).

SatBooth talks to the hub only through **Nostr Wallet Connect** (NIP-47), with two connections:

| Connection | NWC methods | Purpose |
|---|---|---|
| `invoice` | `make_invoice`, `lookup_invoice`, `list_transactions`, `get_info`, notifications (`payment_received`) | Taking payments. This connection **can't pay**. With the `get_info` scope, which SatBooth asks for (plan 14), the hub states its network. The `make_invoice` scope also grants the hold-invoice methods `make_hold_invoice`, `settle_hold_invoice` and `cancel_hold_invoice` (`nip47/permissions/permissions.go:126`, getAlby/hub `921003b`; observed in the connection's 13194, `qualify/REPORT.md`); SatBooth does not use them. |
| `payout` | `pay_invoice` (the hub also grants `pay_keysend`, `multi_pay_invoice` and `multi_pay_keysend` with it, observed in `qualify/REPORT.md`; SatBooth never calls them), `lookup_invoice` (look up a payout after a timeout), `get_info` (the node's network, checked at start), `get_budget` (alert before the budget runs out; `get_info` and `get_budget` are always granted by the hub as methods, `nip47/permissions/permissions.go:195-197`, `921003b`, but the node's fields, the network among them, come only with the `get_info` scope, which SatBooth asks for (plan 14); the startup check refuses any method outside these). A **lifetime budget enforced by the hub** (renewal `never`, raised by a person). | Approved refunds only. |

Facts that shape the design (getAlby/hub main, read 2026-10-26 to 10-30):

- NWC traffic goes over a Nostr relay. Requests, responses and NIP-44 notifications use ephemeral kinds (23194,
  23195, 23197). The hub also sends a NIP-04 copy of each notification as kind 23196, which SatBooth's relay
  drops. The 13194 info event is replaceable and stored.
- The hub subscribes without `since`, dedups requests by event id, and ignores `pay_invoice`/`make_invoice`
  requests older than 6 h. Alby's own relay keeps "ephemeral events until consumed". A standard relay
  keeps them only for a time window or not at all. So **SatBooth must treat NWC as at-least-once with
  timeouts**: on a missing response, re-query with `lookup_invoice`/`list_transactions`.
- **Duplicate `pay_invoice` (plan 4 qualification; Alby Hub v1.24.6, source getAlby/hub `921003b`).** The hub
  refuses a second `pay_invoice` for an invoice that is pending or settled. Observed: "there is already a
  payment pending for this invoice" and "this invoice has already been paid", each with no new row and no
  second HTLC. After a row has gone `failed`, the hub's own check does not refuse (source,
  `transactions/transactions_service.go:325-357`). That path could not be produced on regtest: in 15 hub
  stops and kills the row stayed `pending`. So it is unverified, and LDK's duplicate check is then the only
  guard (source, ldk-node HEAD `src/payment/bolt11.rs:102-108`, not the pinned binary; the hub deletes LDK's
  payment records after 24 h, `lnclient/ldk/ldk.go:2161-2195`).
- **Facts the client relies on.** Each is marked: observed (qualification, `qualify/REPORT.md`) or source
  (`921003b`).
  - Payment errors are `INTERNAL` with free text, never a code that tells the outcome (source,
    `nip47/controllers/map_nip47_error.go:12-32`). Observed texts include "received payment failed event:
    RecipientRejected" after the receiver cancelled a hold invoice. The client never decides an outcome from
    `error.code`; only a lookup does.
  - A `failed` outgoing payment can later settle: the hub settles the newest FAILED row of a hash if LDK
    reports success (source, `transactions_service.go:913-927, 967`).
  - `expired` is also reported for outgoing rows (source, `nip47/models/transactions.go:28-31`).
  - Lookups, lists and notifications are hub-wide for apps that are not isolated: a notification for another
    app's invoice reaches every such app with the `notifications` scope, so match `payment_hash` against
    SatBooth's own invoices (source, `nip47/notifications/nip47_notifier.go:108-116`, `transactions_service.go:606-640`. Observed in
    part: an invoice made through the hub's own API produced `payment_received` on the invoice connection and
    was found by its `lookup_invoice`; `list_transactions` was not tested).
  - A budget of 0 means no budget (source, `transactions_service.go:1196`). `satbooth-pair` refuses 0.
  - The fee reserve counted against the budget while a payment is pending is max(1%, 10 sat) (observed on both sides of
    the floor: 1% of 15,000 sat, and 10 sat for a 500 sat payment; `qualify/REPORT.md` Budget; source
    `transactions_service.go:1142-1145`).
  - `list_transactions` filters and orders by `updated_at`, not creation time (source,
    `transactions_service.go:730-745`).
  - Without the `get_info` scope, `get_info` omits `network` and the block height (observed). The
    `invoice` connection did not ask for it before plan 14: both pairing links now do, so the start-up check of the
    network reads it from each connection.
  - The hub ignores the NIP-40 `expiration` tag, and drops `pay_invoice` and `make_invoice` requests older than
    6 h with no response (source, `nip47/event_handler.go:348-360`).
  - A request from a key that has no app gets no response (observed in the hub log; source
    `event_handler.go:73-82`). A client sees only its own timeout.
  - The hub dedups request ids in its database, which survived a hub restart (observed), but it keeps only
    the newest 1000 (source, `service/service.go:168-178, 297-326`).
  - **Facts plan 5 relies on.**
    - A `lookup_invoice` for a hash the hub has no row for answers `NOT_FOUND` (source,
      `transactions_service.go:652`, `nip47/controllers/map_nip47_error.go:15`). `NOT_FOUND` is the only answer
      SatBooth takes as evidence that an invoice does not exist. For an isolated app the hub also answers it for
      another app's invoice (source, `transactions_service.go:612-628`); SatBooth's connections are not isolated.
      `INTERNAL` (a database error), `RESTRICTED` and `EXPIRED` prove nothing about the invoice.
    - The hub refuses a second payment of the same invoice (observed: HTTP 500 "this invoice has already been paid",
      `qualify/REPORT.md`, `TestLightningReplay`).
    - The hub's own REST `GET /api/transactions/{hash}` still read `pending` after the invoice expired, while the NWC
      lookup read non-settled (observed, `TestLightningExpireBefore` and `After`). SatBooth reads only the NWC
      lookup and treats every state but `settled` as unpaid.
    - From the start of the customer's payment to the order reading `paid`: 0.41 s (observed, `TestLightningPay`, one
      development host; the notification path is not told apart from the poll, see `qualify/REPORT.md`).
- **Pairing.** SatBooth makes its own client key. `satbooth-pair` writes it to a new 0600
  file and prints a link to the hub's own new-connection page, with the public key, name, methods and budget
  filled in. The operator opens the link, checks the methods and budget, and clicks Connect. They copy the
  connection's "Wallet Public Key" from "Connection Details" into SatBooth's configuration and the relay's
  pairs. The secret never leaves SatBooth, and the hub shows none.
- The hub can't authenticate to a relay: it has no NIP-42 handler and sends no custom headers. So relay
  access control must be the network, or a pubkey allowlist on the relay.
- Settings for a hub with no third-party dependencies (`config/models.go` and callers):
  - `RELAY`: our own relay (see 2.4), not the default Alby relays.
  - Chain source: `LDK_BITCOIND_RPC_*` pointing at our own bitcoind, instead of the default
    `electrum.getalbypro.com`.
  - `LDK_GOSSIP_SOURCE` empty: P2P gossip from the peer, no rapid gossip sync (RGS) server.
  - Never link an Alby account. That means no VSS (vss.getalbypro.com) and `AUTO_LINK_ALBY_ACCOUNT=false`.
  - `SEND_EVENTS_TO_ALBY=false`.
  - `MEMPOOL_API`: our own mempool/esplora instance, or accept the rare call.
  - No LSPs, Boltz or swaps.
  - As built in the hub bundle (plan 10, §3): with bitcoind as LDK's chain source, the hub still sends its on-chain
    address lookups (its address API and swaps) to an Electrum server, Alby's by default (getAlby/hub v1.24.6
    `chain/address_lookup.go:20-26`). The bundle points `LDK_ELECTRUM_SERVER` at its own Fulcrum. `MEMPOOL_API` is
    left at the hub's default; the operator may set it in `hub.env`.
- Hard-coded `getalby.com/api` calls remain for fiat rates, channel suggestions at startup and service
  status. **Catch:** `api.Health` calls `getalby.com/api/internal/info` first and fails when that fails, so
  health checks must use NWC `get_info` or node/channel state instead.
- Back up the hub ourselves: an encrypted backup of its data directory, plus the static channel backup.
  Optionally, self-host channel state in a VSS server (lightningdevkit/vss-server). That needs a small
  hub change, because the hub fetches a VSS token from Alby OAuth today.
- Test once on a throwaway hub behind an **egress firewall** that allows only the relay, bitcoind and the
  channel peer, so any missed outbound dependency fails visibly. Cover: start, accept the channel,
  invoice, pay, notification, payout, restart, reconcile.

SatBooth supports one Lightning backend: a dedicated Alby Hub over NWC. It does not support phoenixd
(github.com/ACINQ/phoenixd, read from source at commit `0a897b4`): its only peer is ACINQ's LSP, hard-coded in
`conf/Lsp.kt`. Its HTTP API also has no spending limit (the full-access password can empty the node, on-chain balance
included); with the limited password only, a person would pay refunds, as for on-chain and Monero. It does not drive a
main node's API directly (eclair or LND: the API password is all-or-nothing, so code execution in SatBooth would mean
full control of the main node and all its funds), run as a plugin inside the main node (the same exposure, plus the
crash coupling and a rebuild on every node upgrade), or implement a Lightning node of its own on LDK (key handling,
channel monitor persistence, chain sync, fee estimation, force-close reserves, backups and the API churn of LDK 0.x;
Alby Hub already is that layer).

### 2.2 On-chain bitcoin: watch-only

- The merchant gives SatBooth a **watch-only output descriptor** (`wpkh(xpub/0/*)` or `tr(...)`), never a
  seed or xprv. SatBooth refuses a descriptor that contains private key material (a test: `xprv`, WIF, `tprv`).
- One fresh address per invoice. Derive it from the descriptor and record the index before showing it.
  Watch the gap limit (unpaid invoices leave unused addresses) and plan how the merchant's wallet will
  find later payments.
- **Settled in plan 6:** an address derived with `deriveaddresses` past the range imported into the
  watch-only wallet would go unwatched. So the descriptor is imported as `active` and SatBooth calls
  `getnewaddress` on the `disable_private_keys` wallet, which watches what it hands out and extends its own range
  (observed, `qualify/REPORT.md`); `getaddressinfo` gives the index, and `deriveaddresses` is only the
  independent cross-check (§3, "On-chain payments, as built").
- Detection: bitcoind's own descriptor wallet in watch-only mode
  (created with `disable_private_keys`, then `importdescriptors`, `listsinceblock`/`gettransaction`). No
  NBXplorer: SatBooth already relies on bitcoind for descriptors and addresses.
- Confirmation policy per amount (e.g. 0-conf display only, 1 conf for small amounts, more for large).
  Handle RBF (an unconfirmed payment can be replaced) and reorgs (a confirmed payment can disappear). As built, tiers
  by amount; since plan 6.1 a tier may be 0, which pays from the mempool and trusts the payer (§3, plan 6.1).
- **Late payments, on-chain and Monero.** The expiry is for the quote:
  - a payment counts if it is first **seen** (mempool or tx pool) before expiry, and its confirmations
    may arrive later;
  - each address and subaddress is still watched for 7 days after expiry;
  - a payment first seen after expiry gets status `late` and waits for a person, who credits or refunds
    it on the same rail. It is never fulfilled automatically.
  - a payment first seen before expiry counts even if it drops out of the mempool and is mined later, so on-chain
    `order.paid` can follow `order.expired` or `order.underpaid` inside the watch window, with an `ALERT`, as for
    Lightning's late settlement in §3; since plan 6.1 also after the window, for an on-time payment still tracked then.
  Monero's 10-block lock alone takes about 20 minutes, longer than a 15–30 minute expiry.
- Refunds: a human pays from the merchant's wallet.
- **As built (plan 6):** the descriptor grammar (`descriptor`), bitcoind's watch-only descriptor wallet, addresses by
  `getnewaddress` with the index recorded before the address is shown, confirmations by amount tier, RBF, reorgs,
  partial, over- and late payments, the 7-day window and the gap `ALERT`. How it works is in §3, "On-chain payments,
  as built"; how to set it up is in `docs/bitcoind.md`.

### 2.3 Monero: view-only

- `monero-wallet-rpc` with a **view-only wallet**, created with `generate_from_keys` from the primary
  address and the **private view key** only, with no spend key. Use our own `monerod`.
- One subaddress per invoice (`create_address`). Detect payments with `get_transfers` (`in`, `pool`) /
  `incoming_transfers`. Lock time is 10 blocks.
- A view-only wallet sees incoming outputs, but it can't tell when they are spent, so its "balance" is
  not real. That doesn't matter for payment detection.
- Refunds: a human pays from the merchant's wallet.
- **Plan 8, built.** The details, the implementation choices and the rulings are in §3, "Monero
  payments, as built (plan 8)"; how to set it up is in `docs/monero.md` and `docs/rules-monero.md`.
  1. **wallet-rpc confinement:** full RPC, reachable only over the private link, with an HTTP Digest login whose
     password SatBooth loads from a 0600 file; the threat model (§0) states what the login's holder can do.
  2. **Placement:** monerod and wallet-rpc on a third private host if the operator can afford it, else on the private
     hub host, never on the public SatBooth host; the operator runs them. Since plan 10 the hub host's bundle runs
     them on the hub host with its `monero` profile (§3, "Operations, as built (plan 10)").
  3. **Wallet:** the operator creates the view-only wallet (this replaces the first bullet's `generate_from_keys`)
     and wallet-rpc opens it with `--wallet-file`; SatBooth takes no Monero key, only the expected primary address;
     `docs/monero.md` advises a fresh wallet used only for shop income.

  4. **XMR rates:** Bitfinex joins the providers, so XMR prices in USD from Kraken and Bitfinex under the unchanged
     rules of the threat model's §10 (Kraken and the custom rate API also work); a currency the configured providers
     cannot reach for XMR is a permanent 400 `invalid_currency` (ruling 8-A).
  5. **Default rails:** a fiat order gets Monero only when `rails` names it; the default stays Lightning and on-chain.
  6. **Confirmations:** tiers by piconero amount, 1 / 3 / 10 by default, 0 allowed per tier and reverted on
     `double_spend_seen`; a payment locked by its sender beyond the normal 10 blocks is never counted, it is late.
  7. **Subaddresses:** one per order, allocated on the "pay" press, never reused, never minor index 0, with an
     `ALERT` past a configurable run of unused ones and plan 6's crash protocol.
  8. **Checkout:** a rail picker without JavaScript, "Bitcoin" or "Monero", each with its own QR; the Monero link
     carries `tx_amount` only; the both-rails `ALERT` covers Monero beside either bitcoin rail.
  9. **Late, partial and over-payments:** plan 6 and 6.1's rules carry over unchanged, minus the RBF first-seen
     inheritance.
  10. **Custom rate API:** SatBooth appends `?asset=<A>` (or `&asset=<A>` when the URL has a query).
  11. **Qualification:** monerod and wallet-rpc built from the official release tarball, its SHA-256 pinned from the
      PGP-signed `hashes.txt`, on the repo's digest-pinned Debian base; v0.18.5.4 (not qualified against v0.19).

### 2.4 Nostr relay for NWC

**Relay independence.** SatBooth ships its own small NWC-only relay in this repo, as part of the set, written in this
repo (no code copied from another relay). SatBooth still takes a plain relay URL, so an operator can point it at any
relay that meets the rules below. The starting point was khatru, which gave the structure only (built in plan 3; what it
took is below).

Go relay code checked on 2026-11-02:

| Source | License | Relay code | What it imports |
|---|---|---|---|
| khatru, in `fiatjaf.com/nostr` (pseudo-version 2026-11-01; the relay behind `nak serve`, nak v0.22.1) | Unlicense (public domain) | `khatru/` 2,631 lines, plus the event, filter and envelope code in the core package (5,702 lines) | fasthttp/websocket, xsync, rs/cors, debounce, x/sync; core: easyjson, json-iterator, gjson, fasturl, xhex, x/exp, plus btcec and coder/websocket |
| rely (`github.com/pippellia-btc/rely` v1.5.2) | MIT | 2,402 lines | gorilla/websocket, nbd-wtf/go-nostr, goccy/go-json |

What was actually taken from khatru (plan 3): its structure, and no code. Its connection object with its
subscriptions and cleanup on close, a NIP-42 challenge per connection, and its use of the OK and CLOSED
prefixes. No code transplants: it uses fasthttp/websocket and easyjson, handles each message in its own
goroutine, stores no ephemeral events and answers `mute:` when nobody listens. The relay (`relay/`) is written
on the `nostr` package and `coder/websocket` (the only new dependency), with the standard library otherwise.
Messages on a connection are handled in order by one reader goroutine; one writer goroutine drains a bounded
queue, and a full queue closes that connection. The relay holds no secret key: it only verifies.

Built as follows:

1. **Kind 5 (deletion) from a wallet key is refused** with `blocked: deletion is not supported`. The hub
   publishes it when an app is deleted, and only logs the failure. Removing the pair from the relay's
   configuration is what revokes a connection.
2. **The relay is memory-only, and delivery is guaranteed end to end** (see "Delivery guarantees" below).

Implementation choices:

- a separate binary, `cmd/satbooth-relay`, with one JSON configuration file;
- plain `ws://` on the private listener, behind the operator's WireGuard or firewall;
- when the window is full, the oldest event is evicted and a valid one is never refused: the hub never
  retries a refused 23195 and drops a refused notification;
- a default window of 1 hour.

**Clocks.** The relay compares `created_at`, NIP-40 `expiration` and NIP-42 AUTH times against its own clock.
NTP is required on every host (the relay's, the hub's and SatBooth's). AUTH tolerates `nostr.AuthMaxSkew`;
`created_at` may be at most that far in the future.

**What the relay does** (`relay/`):

- Two listeners. The hub listener must be on loopback; it needs no login. The client listener sends
  `["AUTH", <challenge>]` at connect and closes a connection that has not authenticated within 10 s
  (`AuthDeadline`). AUTH is checked against the configured `client_relay_url` byte for byte, never against
  `Host`. Up to 4 client keys may authenticate on one connection.
- Admission is by pair (`wallet`, `client`) from the configuration, in this order: listener rule, grammar
  (`nostr.ValidateNWC`), pairing (author and `p` tag), `created_at` bounds, expiration, signature, duplicate.
  Refusals carry the NIP-01 prefixes `invalid:`, `restricted:`, `auth-required:` and `blocked:`, never `mute:`.
  A 23196 from a paired wallet is answered `OK true` and dropped unread. An event over `nostr.MaxEventJSON`
  gets a NOTICE; a frame over `MaxMessage` (135168 bytes) closes the connection with 1009.
- The window holds 23194, 23195 and 23197 for `window_seconds` from arrival (default 3600, range 60 to 21600), capped at
  `window_max_events` and `window_max_bytes`; at a cap the oldest event is evicted. The newest 13194 of each wallet is
  kept outside the caps until a newer one replaces it. Any event carrying an `expiration` tag (the grammar allows it
  only on 23194) whose expiration has passed is never served or broadcast. `Live` is checked just before each write; an
  expiry that passes during the write, or while the frame sits in TCP buffers, is covered at the ends, not by the relay:
  the hub does not check `expiration` (getAlby/hub `921003b`), so SatBooth treats a timed-out request as unresolved,
  resolves it with a lookup, and retries a payout only with the same invoice, which the hub refuses to pay twice (see
  "Delivery guarantees" below). A REQ gets the matching window events in `(created_at, arrival)` order, then EOSE, then
  live events.
- Filters accept only `kinds`, `authors`, `#p`, `#e`, `since`, `until` and `limit`, scoped to configured keys.
- Limits (`relay/limits.go`): 32 connections per listener, 16 subscriptions per connection, 4 filters per REQ,
  a 256-frame write queue per connection, a per-connection cap on the backlog of pending events, a 10 s write
  timeout, and 30 s pings. Idle connections are never closed.
- Logs (`log/slog`, text on stderr) hold connection open and close and refusals by prefix and kind, never
  content, tags or whole events.

**Known gaps.**

- A relay restart loses the window and the 13194 events. The hub republishes 13194 only at startup and when an
  app is created or updated.
- The hub's resubscribe uses `since` = the moment it *noticed* the drop, up to about 90 s after it, so requests
  SatBooth published in that gap are not replayed. Plan 4's lookup after a timeout is the guard.
- The window remembers only the ids it holds. An event evicted by the caps and sent again is accepted and
  broadcast again. That is safe: the hub records each request event id and refuses one already processed
  (getAlby/hub `921003b`, `nip47/event_handler.go:57-72`).

#### Delivery guarantees

The relay may lose messages, by design. Persisting the window would not close the other gaps: the hub's
resubscribe `since` is late, the hub drops a notification it failed to publish, and frames in flight
are lost in a crash. So the guarantee lives at the ends, which keep durable records (the hub's database and
SatBooth's ledger). The relay is a lossy pipe. Requirements on plans 4, 5 and 9 (refunds):

- every 23194 carries `expiration` and has a timeout;
- after a timeout SatBooth resolves the outcome with a lookup and never assumes failure;
- a lost invoice request is resent; an extra unpaid invoice is harmless;
- a payout's pending ledger transfer stays open until a lookup settles it, and a retry uses the same invoice.
  Plan 4's qualification confirmed that `lookup_invoice` finds outgoing payments (`pending`, `settled` and
  `failed` observed). The hub refuses a second `pay_invoice` while the first is pending or settled
  (observed). It does not refuse after a `failed` row (source), and a payout the hub marked `failed` because it was
  shut down mid-payment can still settle after its restart (source). A refund (built, plan 9) therefore sends
  `pay_invoice` once, ever, and then only looks it up: a hub-reported `failed` ends it, while a refund the hub has no
  record of, or shows as `expired`, stays open until a person's signed void ends it (threat model §4). A payout in
  flight across a hub stop or kill reads `pending` after the restart, the hub refuses a retry while it is pending,
  and the payout settles once the receiver settles it, once on each side (observed, `qualify/REPORT.md`, "Payouts in
  flight");
- the client's request timeout defaults to 70 s, longer than LDK's 50 s payment retry (source, ldk-node HEAD
  `src/config.rs:60`; not measured with the pinned binary). A 23194's
  `expiration` is `created_at` plus that timeout, so the relay does not deliver a request SatBooth has stopped
  waiting for; a refund's `pay_invoice` instead expires at its stored `sent_at` plus `payout.timeout_seconds`, so
  the 11-minute void wait always outlasts it. **The client never resends a request itself**: after a timeout the
  caller looks up the outcome;
- incoming payments are found by polling every open invoice until it expires; notifications are only the fast
  path;
- nothing reads 13194 at runtime.

#### Running the relay

`satbooth-relay -config <path>` serves until SIGINT or SIGTERM, then stops within 5 s. See `docs/running.md`.

Two facts for whichever source is used:

- **`fiatjaf.com/nostr`'s serialiser doesn't follow NIP-01.** `appendJSONString` in `event.go` escapes only
  `"`, `\`, `\n`, `\r` and `\t`. NIP-01 (nostr-protocol/nips `01.md`) also requires `\b` and `\f`. An event
  containing those characters gets a different id depending on which library computed it.
- **An NWC-only relay can avoid that whole problem.** Each kind has a fixed grammar for content and
  tags (getAlby/hub `921003b`: `nip47/publish_nip47_info.go`, `event_handler.go`,
  `notifications/nip47_notifier.go`), and no control character is allowed anywhere. The id is then the
  same in every correct implementation. The relay validator and SatBooth's serialiser both enforce this:

  | Kind | Content | Tags |
  |---|---|---|
  | 13194 info (hub) | plaintext method names separated by single spaces, `[a-z_]+( [a-z_]+)*` | `["encryption", <names>]`, `["p", <hex64>]`, optional `["notifications", <names>]`, `["extensions", <names>]`; `<names>` are `[a-z0-9_]` words separated by single spaces |
  | 23194 request (SatBooth) | NIP-44 v2 payload (standard base64) | `["p", <wallet hex64>]`, `["encryption", "nip44_v2"]`, optional `["expiration", <decimal>]` (NIP-40) |
  | 23195 response (hub) | NIP-44 v2 payload | `["p", <hex64>]`, `["e", <hex64>]` |
  | 23197 notification (hub) | NIP-44 v2 payload | `["p", <hex64>]` |
  | 23196 legacy notification (hub) | NIP-04, never parsed | the relay answers OK and drops it unread |
  | 22242 NIP-42 AUTH (SatBooth) | empty | `["relay", <url>]`, `["challenge", <string>]` |

  Anything outside the table is refused. Tag order is as the hub writes it; the validator accepts the
  tags in any order but no unknown ones.

What the relay has to do, from getAlby/hub `921003b` (`nip47/models/models.go`, `service/start.go`):

- Kinds: 13194 (info, replaceable: keep the latest per pubkey), 23194 (request), 23195 (response),
  23197 (NIP-44 notification). Refuse every other kind, except 23196 (legacy NIP-04 notification):
  answer OK, then drop it without storing or forwarding. SatBooth uses NIP-44 only, but the hub
  publishes every notification twice, NIP-04 first and then NIP-44, whatever the connection uses
  (`nip47/notifications/nip47_notifier.go`). A refused NIP-04 copy does not block the NIP-44 one:
  the hub logs the failure and sends the next copy (`nip47_notifier.go:137-145`), but the relay doesn't rely on it. SatBooth always sends
  `["encryption","nip44_v2"]`; without that tag the hub defaults to NIP-04 (`event_handler.go`).
- Writers: only the allowlisted pubkeys, which are the hub's wallet key for each connection plus SatBooth's
  client key for each connection. Verify each event's id and signature.
- The hub subscribes with `{"kinds":[23194],"#p":[<wallet pubkey>]}` and no `since`, and reads 13194 with
  `{"kinds":[13194],"authors":[<wallet pubkey>],"limit":1}`. Serve only filters scoped to allowlisted keys.
- Keep ephemeral events in memory for a bounded window (the role of strfry's
  `ephemeralEventsLifetimeSeconds`), so a reconnecting side still gets what it missed. The hub dedups
  requests by event id and ignores old `pay_invoice` requests (§2.1).
- Not needed: NIP-42 AUTH for the hub (it can't do it), COUNT, search, imports, payments, storage beyond
  that window, any UI.

Other ways to run a relay for NWC:

- **strfry** (github.com/hoytech/strfry, C++, GPL-3.0, widely deployed). It binds to `127.0.0.1` by
  default. A `writePolicy.plugin` script can accept only the two parties' pubkeys and the NWC kinds.
  Ephemeral events are kept for `ephemeralEventsLifetimeSeconds` (default 300; raise it to around 3600),
  and events older than `rejectEphemeralEventsOlderThanSeconds` (default 60) are rejected, so the hosts'
  clocks must be in sync. It needs no code.
- **A private instance of an existing relay you already run**, with write allowlists by pubkey and kind,
  and short forced expiry on stored NWC events. This gives a better "kept until consumed" behaviour, at
  the cost of a code change per allowlist edit.

Whichever is chosen, reads should be limited to filters scoped to the allowlisted keys.

**Link to SatBooth.** The relay has two listeners:
- `127.0.0.1` for the hub, with no login, because the hub can't do NIP-42;
- a private address for SatBooth that requires NIP-42 AUTH from SatBooth's allowlisted client keys before
  it accepts any REQ or EVENT. Khatru's NIP-42 flow was the model.

The operator also keeps that address behind WireGuard or a firewall (their job, not this repo's), so a
wrong firewall rule still doesn't let a stranger read metadata or flood subscriptions.

## 3. Shape of the service

- **Runtime and language: Go, self-hosted.** The current point release, `govulncheck` on
  each Go release. In the four weeks to 2026-10-30, ~120 of 135 new Go vulnerability entries were in
  third-party modules, so the standard library comes first. A pure-stdlib NWC client isn't possible (no
  secp256k1, no public ChaCha20, no WebSocket client): the proposed minimal set, its measurements and the
  supply-chain controls are in [`dependencies.md`](dependencies.md).
- **Go module path: `github.com/satbooth/satbooth`.** Packages live at the repo
  root (`nostr/`, `nip44/`, ...), never under a top-level directory named `internal`.
- **Storage: SQLite**, through the pure-Go driver `ncruces/go-sqlite3` (no cgo). TigerBeetle if a large setup ever needs
  more, not Postgres. See `dependencies.md` §6, including a proposal to model money as immutable transfers from the
  start. Orders, invoices per rail, payment events, an append-only audit log. The ledger (`ledger/`) is built (plan 2):
  accounts and transfers (single-phase, pending, post, void, expiry) in TigerBeetle's model, insert-only and guarded by
  triggers, with restart, property and fuzz tests.
- **Where the public surface runs:** a CDN/WAF in front of everything public. The hub, relay, bitcoind, Fulcrum,
  monerod and wallet-rpc are never public.
- **Pricing:** either a fiat price converted by `rates` (plan 7) or explicit amounts from the merchant's app.
- **Who calls SatBooth:**
  - orders are created only by the merchant's app, server to server, with a per-capability API key. A
    public tip page with no app behind it is not in v1;
  - the `known`/`new` class comes from SatBooth's own payment records for the account id that the app
    sends;
  - the challenge before "pay" is the CDN's own (§1 of the threat model). SatBooth builds no CAPTCHA and
    calls no CAPTCHA vendor (principle 4).
- **Purging expired invoices from the hub** (§5, question 6) is the operator's job. SatBooth never
  touches the hub's database.

### Lightning payments, as built (plan 5)

Packages: `qr`, `store`, `limits`, `lnpay`, `webhook`, `api`, `checkout`, `memlock`, `service` and the binary
`cmd/satbooth`. How to run and configure it, the API and the webhook recipe are in `docs/running.md`,
`docs/configuration.md`, `docs/merchant-api.md` and `docs/webhooks.md`. Numbers below are
constants in the code; the constant is named where it helps.

**Decisions.**

1. **Order state is immutable, and "paid" is a ledger transfer.** Orders, invoices and webhook events are insert-only
   rows. An order is paid when the ledger holds the transfer whose id is derived from its invoice's payment hash. Its
   status (`new`, `paid`, `expired`) is derived from those rows, the ledger and the clock; no row is ever updated,
   except the schema version row of each database and, since plan 10, the one `pair` row of each.
2. **The merchant API is on a separate private listener**, on a specific IP (WireGuard or a private network), with
   per-capability API keys as a second layer. Only the checkout listener is public, behind the CDN/WAF.

**Implementation choices.**

- **Orders have their own SQLite file** (`store.db`), beside the ledger's (`ledger.db`). The ledger owns its file's
  `schema_version` table and runs as a single writer. Nothing needs a transaction across the two files: "paid" is
  the ledger transfer's id, which can exist once.
- **The checkout page has no JavaScript.** It is server-rendered HTML that refreshes itself with
  `<meta http-equiv="refresh">` (5 s), under a strict content security policy. The QR code is an SVG served by
  SatBooth (`qr/`, our copy of `rsc.io/qr`'s encoder, `dependencies.md` §6 item 5).
- **One open invoice per order.** "Pay" reuses the open invoice. A new one is made only after the old one expires
  and only while the order is open. Invoice expiry is 20 minutes by default (`lnpay.DefaultInvoiceExpiry`),
  configurable from 15 to 30. The order has its own expiry, set by the merchant's app (`expires_in`, 300 to
  86,400 s, default 3,600 s).
- **Descriptions carry SatBooth's own opaque order id** (`SatBooth order <id>`), never the merchant's reference or
  an account id (threat model §8).
- **Whole sats only.** An order amount is a multiple of 1,000 msat, up to 2^53, because the hub refuses partial sats
  (source, `transactions_service.go:177-182`).
- **Webhooks are at-least-once hints.** The header is `SatBooth-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>`,
  where the MAC covers `t`, a dot and the body, keyed with the raw bytes of the secret (the file holds them as hex).
  Retries back off from 10 s, doubling up to 1 h (`webhook.RetryDelay`), for 24 h (`webhook.RetryWindow`). The
  receiver re-reads the order from the API.
- **Outcomes are resolved by lookup.** An `nwc` error in the never-sent list (`nwc/doc.go`) means no invoice was
  made. Any other error means the outcome is unknown: the customer can press "pay" again, and an extra unseen
  invoice on the hub is harmless. `ErrNoResponse` is checked before the context errors, because a timed-out call
  matches both.
- **The global limits have defaults** in the configuration: 1 creation per second with a burst of 10, 1,000 open
  invoices with the `new` class capped at 20 % of them, 5,000 attempts a day, and a daily amount and a largest amount
  that the operator must set (`limits.Config`). The minimum is 1 sat. None is per IP; plan 6.1 adds
  `max_open_per_account`, the one per-account limit (§3, "On-chain follow-ups, as built (plan 6.1)").
- **A fixed global polling rate**: the reconciler looks up open invoices at 5 lookups a second in total
  (`lnpay.DefaultPollRate`), so load on the hub does not follow traffic. Notifications only move an invoice to the
  front of the queue.

**Rules of the payment path.**

- **Paid is one transfer.** Its id is `PaidTransferID`: the first 16 bytes of SHA-256 of the string
  `satbooth/ln-paid/v1` followed by the 32 raw bytes of the payment hash. It debits the account `LightningHub` and
  credits `MerchantLightning`, in msat. `LightningHub` has the flag credits-must-not-exceed-debits and
  `MerchantLightning` debits-must-not-exceed-credits, so a bug in a later payout path cannot overdraw what was paid
  in. A settlement seen twice (notification and poll, or after a restart) creates the transfer once, because its
  id is fixed. A second invoice of an already paid order that is also paid is recorded (the money arrived) and
  logged with `ALERT`; the order is paid once and its event exists once.
- **An invoice is closed only on evidence** (`lnpay` package comment).
  - After the invoice's expiry plus a grace of 2 minutes (`lnpay.DefaultGrace`), a successful lookup that shows the
    invoice not settled closes it, and the order can read `expired`.
  - Among error answers, only the wallet's `NOT_FOUND` is evidence, and only past the deadline, 24 hours after the
    later of the invoice's expiry and the service's start (`store.DeadlineGrace`); it closes the invoice and logs
    `ALERT`.
  - Nothing else closes an invoice, however long it lasts: not `INTERNAL`, `RESTRICTED` or `EXPIRED`, a malformed
    answer, a row for another hash or type, a settlement below the amount, or a transport failure. None proves it
    unpaid. Past the deadline such an invoice becomes stale: it stops counting against `max_open`, is looked up
    every 10 minutes, logs `ALERT`, and its order keeps reading `new`. It does not count again after a restart (the
    recount at startup leaves out every invoice past its own expiry plus 24 hours), but a restart moves the deadline
    to 24 hours after the start, so until then the invoice is looked up at the normal rate.
  - **Late settlement (keep watching 24 hours).** A Lightning node accepts a
    payment for a while after an invoice's expiry. In rust-lightning (`main` as of 2026-11-03, read raw from GitHub),
    `lightning/src/ln/inbound_payment.rs` `calculate_absolute_expiry` returns `highest_seen_timestamp +
    invoice_expiry_delta_secs + 7200`, where `highest_seen_timestamp` is the latest block-header time seen, and the
    doc of `create_inbound_payment` in `lightning/src/ln/channelmanager.rs` says it will "accept a payment and
    generate a `PaymentClaimable` event for some time after the expiry". The window is therefore about 2 hours past
    the nominal expiry, longer or shorter by how far block-header times lag the clock at creation and at arrival.
    The LDK in a given hub may be another version. So the close stays as it is (the order reads `expired`,
    `order.expired` goes out), and an invoice closed by lookup is still looked up on the slow poll (10 minutes,
    through the same paced round robin) until its deadline, then dropped without a line, after completing a payment
    the ledger already holds. A watched invoice that is not due costs a round nothing; the due ones share the lookup
    rate with the open invoices, at most max(open invoices, 50) per round. The deadline is max(ExpiresAt, the
    service's start) + 24 hours, so a restart inside it extends the watch to 24 hours after the start, up to about
    48 hours after the expiry. The cost is about 144 lookups per watched invoice per day; past about 600 × PollRate
    watched invoices (3,000 at the default 5 a second) every round carries the cap, and each gets fewer. A
    settlement for the full amount is recorded through the normal paid
    path (the transfer, `order.paid`, the `account_paid` fact) with an `ALERT` line, and the order reads `paid`,
    because `Status` reads the ledger before the closures. A short settlement or a bad row alerts as for an open
    invoice; an error changes nothing. A watched invoice holds no `max_open` slot and does not stop a new invoice
    for its order. After a restart the watch is re-derived from the store
    (`store.InvoicesClosedByLookup`: closed by lookup, own expiry plus 24 hours not passed), paid ones included, so a
    crash between a late payment's transfer and its event is completed; it is spread over one slow-poll period,
    newest expiry first, rather than entering the first round at once. `order.paid` is final for the merchant, and
    can follow `order.expired` by up to 24 hours (about 48 after a restart in between).
  - The reconciler looks an invoice up before it closes it, so a payment made while SatBooth was down, however
    long, is recorded on the first pass after the restart.
- **Pressing "pay"** writes the attempt row, then launches the MakeInvoice call on a flight detached from the
  customer's request, so closing the page does not abandon a call that was counted. Before a second invoice is made
  for an order, "pay" looks up that order's expired but unclosed invoices (bound 15 s each): if one settled, it is
  recorded and the order reads `paid`; if a lookup fails, "pay" is refused as outcome unknown. What remains is a
  payment that settles after the lookup said "not settled" (for example one in flight at the invoice's expiry) and
  after a second invoice was made: both settle, both are in the ledger, and the `ALERT` above says so.
- **Limits are checked under one short mutex** with the counts, so two requests cannot both pass the open-invoice cap.
  The mutex is not held across a per-request store call: the counts are kept in memory under it, and only the daily
  figures are read from the store under it, once a day (`AttemptsSince`). Unused `new` slots are lent to `known`
  accounts (`limits.Admit`): a flood of `new` requests is still capped at its share, but `known` traffic can fill all of
  `max_open` and leave no room for `new` customers. Becoming `known` takes a paid order, so this costs an attacker
  money. A `max_open` below 5 with the default share, or any share that leaves `new` no slot, is refused at startup.
- **Logs** carry fixed messages and opaque ids only (order or event ids); no key, secret, invoice, payment hash or
  wallet-supplied text.

**Operation.**

- Startup is fail-fast when the wallet relay is unreachable: the first connection is not retried (30 s at most), and
  the process exits 1. The operator runs it under a supervisor (systemd `Restart=on-failure`, `RestartSec=5`; the
  public host's Compose bundle uses `restart: unless-stopped`, plan 10). After the first connection the NWC client
  reconnects by itself.
- **Keys never reach swap** (`memlock/`, `service.Lock`). On Linux, before any secret file
  is opened, the process calls `mlockall(MCL_CURRENT|MCL_FUTURE)` and sets `PR_SET_DUMPABLE` 0 and `RLIMIT_CORE` 0.
  If locking fails it refuses to start unless `allow_swap` is true (for encrypted or absent swap); failing to disable
  core dumps is always fatal. The operator needs `LimitMEMLOCK=infinity` or `CAP_IPC_LOCK`. A fixed limit such as
  64 MB fails: the locked size follows the Go runtime's reserved address space, not the much smaller resident size.
  On macOS nothing is locked and the service says so (development only).
- Startup order, for anything that embeds the service: `service.ReadConfig`, `service.Lock`, `Config.CheckFiles`,
  `service.Run`.
- **Server limits** on both listeners (`service` constants): `ReadHeaderTimeout` 5 s, `ReadTimeout` 10 s,
  `WriteTimeout` 15 s, `IdleTimeout` 60 s, headers at most 8 KiB (exact: a request over it gets 431), bodies at most
  16 KiB, 256 requests in flight (the rest get 503), and a 10 s grace at shutdown to finish requests and the
  invoice requests to the wallet whose presser went away (`lnpay.Service.WaitFlights`); one still running then is
  abandoned with a log line.
- **Outbound calls** go only to the configured wallet relay and the configured webhook URLs (a list since plan 13,
  below), each with a timeout (the NWC timeout, 70 s by default; 10 s for a webhook). The webhook client follows no
  redirect and uses no proxy from the environment. Since plan 7, with the `rates` block, also to the configured rate
  providers (10 s each; see "Fiat pricing, as built (plan 7)" below); since plan 6, with the `onchain` block, to
  bitcoind over the private link.
- SIGHUP reloads the API keys; a refused file leaves the old keys in force.

### On-chain payments, as built (plan 6)

Packages: `btcrpc` (the bitcoind JSON-RPC client), `descriptor` (the intake grammar), `onchain` (addresses, the startup
checks and the watcher; `onchain/onchaintest` is an in-memory bitcoind for tests) and `status` (one order status from
both rails), with changes in `store`, `lnpay`, `limits`, `api`, `checkout`, `webhook` and `service`. The bitcoind
setup, the configuration, the API fields, the events and the rules are in `docs/bitcoind.md`, `docs/configuration.md`,
`docs/merchant-api.md`, `docs/webhooks.md` and `docs/rules-onchain.md`. Claims about Bitcoin Core 32.1
are marked as in §2.1: *source* or *observed*
(`qualify/REPORT.md`, "On-chain end to end").

**Decisions.**

1. **Confirmations by amount tier**, in sat, in the configuration: 1 up to 1,000,000 sat, 3 up to 10,000,000, 6 above
   by default. The page shows "Seen, N of M confirmations". A tier may be 0 (plan 6.1; until then 0-conf was never
   paid).
2. **bitcoind is the hub host's node** over the private link. SatBooth has its own RPC user, confined by `rpcwhitelist`
   with `rpcwhitelistdefault=0`. The whitelist is per method and reaches every loaded wallet (observed).
   Plan 6.1 recommends a bitcoind dedicated to the hub and SatBooth, with no wallet that holds keys loaded on it.
3. **One unified QR**, the BIP321 URI `bitcoin:<ADDRESS>?amount=<btc>&lightning=<BOLT11>`, with copy text for both and
   an "Open in wallet" link with the same URI, without JavaScript.
4. **A reorg after paid** turns the order back to unpaid, sends `order.reverted` and logs an `ALERT`. If the payment
   confirms again the order reads paid again, with no second `order.paid` for it.
5. **Gap limit:** no address is ever reused. The merchant raises their wallet's gap limit, and SatBooth logs an `ALERT`
   past a configurable run of unused addresses (`gap_alert`, default 1,000).
6. **Descriptors:** only `wpkh(KEY/0/*)` and `tr(KEY/0/*)`, one key, with an optional key origin; `tpub` off mainnet.
   Anything with private key material is refused with a fixed error, before any RPC call, and never logged.
7. **Partial payments:** every payment first seen before the expiry is summed. At the amount the order is paid; above
   it also overpaid, and a person refunds the excess. Short at the expiry it is underpaid and waits for a person.
   §2.2's late rules apply.
8. **Plan 5 leftovers, built here:** the "Open in wallet" link; unused `new` slots lent to `known` accounts
   (`limits.Admit`); at startup, completing closed Lightning invoices that have a ledger transfer but no `order.paid`
   (`lnpay` `healPaid`, for invoices closed in the last 7 days, `lnpay.HealWindow`).
9. **Fiat pricing is plan 7** (§1), Monero plan 8, Lightning refunds plan 9 and operations plan 10.
10. **Monero must fit without rework:** an amount per rail in the order model, the shared statuses and rules, and the
    watcher's marks, window and status precedence kept rail-generic where that cost little. Nothing is built for
    Monero.

**Rulings made during the build.**

- A. **Migration 1 is edited, not extended.** An on-chain-only order needs `amount_msat` 0, which SQLite can only allow
  by rebuilding `orders`; nothing is deployed, so the schema of migration 1 changed instead, and `store.Open` refuses a
  file from before plan 6 (since plan 8, from before the Monero tables) with `store: the database is from before the
  Monero tables; this version does not upgrade it; start with a new store and a new ledger (both database files)`. An
  edit like this is allowed only while no release exists.
- C. **`order.paid_again`**: a reverted payment that confirms again adds `order.paid_again` with the
  `seq` n of the latest `order.reverted` (id `EventID("order.paid_again", orderID, n)`), never a second `order.paid`.
- D. **A fee bump (RBF) is a new output** with its own first-seen time: the replaced output goes
  `dead`, and a replacement first seen after the expiry is `late`. Since plan 6.1 (decision 8) a replacement that
  pays the same address exactly what an on-time transaction paid it inherits that transaction's first-seen time, when
  it is first listed inside the order's watch window (decision 9); D covers only the other replacements.
- E. **`order.underpaid` and `order.late`** exist, once per order: "waits for a person" needs a push.
  `order.late` is added when a late output's forward is recorded (Ruling Z2), not when it is first seen.
- F. **The operator creates the wallet** and imports the descriptor with `bitcoin-cli`
  (`docs/bitcoind.md`); SatBooth's one RPC user has no `createwallet` or `importdescriptors`, and SatBooth checks the
  wallet at every start.
- G. **A reorg of `SettledDepth` blocks or more** (the largest tier, 6 by default) after an order was
  paid is not detected: a payment recorded that deep is stored as settled and not polled again, in any process
  (threat model §3).
- I. **Fail fast at boot:** bitcoind unreachable or a failed startup check exits 1, as for the wallet
  relay; a runtime outage is ridden out.
- J. **No testnet4.** The networks are `mainnet`, `testnet`, `signet` and `regtest`: testnet4's chain
  name was inferred, not read.
- K. **`order.reverted` and `order.paid_again` follow the order's combined state**: they go out only when
  the order, both rails together, stops or starts reading paid. A reorg under an order paid by Lightning sends no
  event; the reorg `ALERT` is still logged. Lightning's payment record takes the same lock as the watched rails'
  settles, so a payment is recorded either before a settle reads it or after that settle's reversals and events.
- L. **An on-time payment first seen after an outage reads late.** First-seen is SatBooth's clock, so an
  outage of bitcoind or SatBooth that spans the expiry makes it `late`, and it waits for a person (threat model §3).
- M. **Events can arrive out of order**; `docs/webhooks.md` tells the merchant to re-read the order on every event.
- O. **A 78-byte extended key whose key byte (byte 45) is `0x00` is refused as private**, whatever its version bytes.
- P2. **The descriptor's `#checksum` is dropped unverified.** The key has its own Base58Check, and the key origin does not
  change the addresses; bitcoind checks the canonical form at startup (`getdescriptorinfo`).
- Q. **After a reorg puts a payment back in the mempool**, the rail reads `seen` (not `waiting`); past the expiry, where
  its `order.reverted` is an unpaid event, `underpaid`. Since plan 6.1,
  except on a 0 tier, where a recorded 0-conf payment back in the mempool stays paid and only a dead mark reverts it.
- R. **A JSON-RPC error from `listsinceblock` with the stored cursor re-anchors the scan at the tip**, with
  an `ALERT` that names the possible loss; tracked payments keep refreshing (threat model §3). As built it fires on any
  JSON-RPC error (not a transport failure), not only on a lost block, and on an answer too large to read (over 8 MiB);
  never on a timeout.
- S. **`order.paid` after `order.expired` or `order.underpaid` is allowed** (a payment first seen in time,
  evicted and mined after the expiry) and logs an `ALERT`.
- T2. **The first `order.paid` the watcher adds makes the account `known`**: the `account_paid` fact, then `lnpay`'s
  in-memory set (`MarkKnown`), then the event.
- U2. **The API answers 503 `onchain_unavailable`** for an order with an on-chain amount while the `onchain` block is not
  configured, rather than a status without the rail. (It does not depend on bitcoind: the status is read from the
  store and the ledger, and a bitcoind outage shows only as "Checking payment…" on the page.)
- V2. **An open invoice that cannot go into a payment URI** leaves the address-only URI, with one `ALERT`.
- W2. **Lightning-only orders get a `lightning:` "Open in wallet" link** (upper case, checked like the URI's
  invoice), and a QR of `LIGHTNING:<INVOICE>`.
- X2. **That `ALERT` is logged once per invoice per process** (a bounded set), not at every 5 s refresh.
- Y2. **Once on-chain money is seen, the page stops offering Lightning** (the hub cannot cancel an
  invoice) and asks only for the amount still missing.
- Z2. **`order.late` and its `ALERT` wait for the late output's record** (mined at its tier, the
  same rule as a counted payment); the rail reads `late` live. A late output replaced or double spent before it
  confirms sends no event, and a receive after the watch window (never recorded) sends none either.

**Implementation choices.**

- **Rows and the ledger, as plan 5.** One address per order, with its descriptor index; one row per payment output
  (`txid`, `vout`) with SatBooth's own first-seen time (never bitcoind's `timereceived`); a *mark* row
  each time an output's classification changes (`mempool`, `mined` at a height and block, `dead`); a cursor with the
  `lastblock` of each scan, written in the same transaction as the outputs it found. Nothing is updated.
- **"Paid" is a transfer per counted output**, in ledger 2 (sat), from `OnchainWallet` (credits must not exceed debits)
  to `MerchantOnchain` (debits must not exceed credits), when the output has the confirmations of the order's tier.
  Its id is `PaidTransferID`: the first 16 bytes of SHA-256 of `satbooth/onchain-paid/v1`, the 32 raw bytes of the
  txid, `vout` and an epoch (4 bytes big-endian each). A reorg adds a reversal (`satbooth/onchain-reversed/v1`, the
  same epoch), and a re-confirmation the next epoch's transfer. Codes: 2 counted, 3 reversal, 4 late. An order is paid
  on-chain when its counted, unreversed transfers reach `onchain_sat`.
- **The tier is picked by the order's `onchain_sat`**, not by an output or the sum: it protects the value at risk.
- **Late outputs are recorded** when they reach the tier, at least 1 confirmation since plan 6.1 (the money arrived),
  with code 4, and never count toward paid or overpaid.
- **One final event per order.** At the first settle after the expiry with the counted live sum below the amount, and
  the Lightning side expired or absent, the order gets `order.expired` (nothing counted) or `order.underpaid`. Later
  changes move only the API state; `order.paid` and `order.late` are added whenever their condition first holds
  (for `order.late`, a late output's forward recorded at its tier).
  `lnpay` sends `order.expired` only for Lightning-only orders; the watcher owns it for the others. Since plan 6.1 an
  order whose on-time outputs reach the amount but are not recorded when the window closes gets `order.underpaid`
  then (§3, plan 6.1).
- **What reverts.** A recorded output that is no longer mined gets a reversal: back in the mempool, conflicted (a
  negative confirmation count or a mempool conflict) or gone. Since plan 6.1, except on a 0 tier, where a recorded
  0-conf payment back in the mempool stays paid and only a dead mark reverts it. One re-mined in another block keeps its
  record. Of several reversals in one settle, only the one that takes the running sum below the amount carries the next
  `seq`, so a crash between two of them leaves the ledger as the uninterrupted run would. On a 0 tier, reversals that
  would turn the paid order unpaid skip the records whose outputs the pass marked dead after its `listsinceblock` (as
  the order's final event is held: a pass that marks an on-time output dead after its scan writes no final event for the
  order in that pass, and the next pass writes it if nothing replaced the payment): a fee bump that landed between the
  scan and the original's `gettransaction` is listed and recorded by the next pass, which then reverses the original
  with no event, as in one pass; a payment really gone is reversed one pass late. The hold is in memory; after a crash
  the restarted pass's scan follows the dead mark, so it records any replacement before it reverses the original. A
  same-pass bump is not held (the reversal leaves the order paid), and neither is a record that needs a block. While
  bitcoind's wallet is behind its node's mempool after a replacement, the output is not marked dead for up to 60 seconds
  (`behindGrace`, from when the pass first found it replaced in the node, also across a return to the mempool and a dead
  mark; checked when a pass reads the output and again finds it replaced, not in a pass that cannot read it; the
  watcher's decision order, below), and the one pass of the dead mark's hold, are each granted once per paying output in
  a running process (`behindSince` and `heldBefore`, in memory until the process ends; each restart grants them once
  more): an output that left the mempool, came back and leaves again gets no new 60 seconds and no second hold, and only
  what is left of its first 60 seconds applies, so it is reversed in the pass that finds it gone only once those are
  over. A fee bump of such an output that lands between a pass's scan and its read reads `order.reverted`, then
  `order.paid_again` with its `ALERT`. Each new paying output, such as the replacement of a fee bump, has its own two
  waits: a payer who keeps putting in replacements that pay the order and removing them again can keep a 0-tier order
  reading paid, without `order.reverted`, while no paying transaction is in the mempool most of the time; per order the
  bound is 60 seconds plus two passes since a paying output of the order was last in the node's mempool, and it ends
  when one of the transactions confirms. A payment really double spent reverts when the wallet has seen the conflict or
  the 60 seconds are over, whichever is first, plus one pass. A bump the wallet has not processed by then reads
  reverted, then paid again, with no longer wait (a payer could renew it). One transient: a pass that holds a record and
  also records another on-time payment of the order can read `overpaid` for that pass, with no `ALERT` (the overpaid
  check counts only the records that stay), until the next pass reverses or keeps the held one.
- **The address protocol:** `getnewaddress` with the explicit type (`bech32` for `wpkh`, `bech32m` for
  `tr`); `getaddressinfo` must say `ismine`, name the configured descriptor as `parent_desc`, and give an `hdkeypath`
  ending in `0/<index>`; `deriveaddresses` of that index must give the same address; the index must be above every
  recorded one; then the row is stored, and only then is the address shown. A failed check shows nothing and logs an
  `ALERT`; an index not above the record also stops all address issuing until a re-check passes.
- **The address is allocated on the "pay" press**, not at order creation, so orders nobody opens burn no address and
  the press stays behind the CDN's challenge. Concurrent presses for one order share one call, detached from the
  customer's request; allocations for different orders run one at a time under one lock, because the index must rise
  across all orders. A press waits 10 s (`onchain.PressWait`) and then reads "busy". No address is issued for an order
  past its expiry or paid on either rail.
- **The watcher** (`onchain.Service.Run`) makes a pass at once and then every `poll_seconds`. A pass: a re-check when
  the index is behind; `getblockchaininfo`; `getmempoolinfo` (while bitcoind has not loaded its persisted mempool after
  a restart, `loaded` false, no output is marked dead in that pass: an unconfirmed output missing from the mempool is
  skipped with one line, `src/rpc/mempool.cpp:1113`, `src/init.cpp:2130-2136` in Core 32.1); `listsinceblock` from the
  cursor with `target_confirmations` = `SettledDepth` + 1, keeping receives to issued addresses and dropping odd entries
  one by one; `gettransaction` for the tracked transactions, shared by their
  outputs and paced at `poll_rate`, an order whole at a time, in turn from where the last pass stopped, with no new order
  started after `poll_seconds` of real time. An order not reached is not settled and reads stale after 3 ×
  `poll_seconds`; it is held (no final event is written or reported for it, and the other rail of a mixed order holds
  its own as it does for an output it cannot judge), and, if the scan touched it, it is touched again in the next pass.
  One with more due transactions than one pass reads, `poll_seconds` × `poll_rate` and at least 8, is not read and has
  an `ALERT` (the sign
  of the confirmations first, then the mempool conflicts, then `getmempoolentry`, because 0 confirmations also means evicted or
  replaced). Since plan 6.1 an output found in neither is read again before it is marked dead: `getmempoolinfo` (bitcoind may have restarted
  inside the pass; loading means wait, *source*: Core 32.1 `src/rpc/mempool.cpp:1113`, `:1143`), `getmempoolentry`, then
  a second `gettransaction` (a block between the reads took it out of the mempool and mined it: `ConnectTip` removes a
  block's transactions and moves the tip under the mempool's lock, `src/validation.cpp:3019-3020`, `:3082`, `:3086`,
  `src/rpc/mempool.cpp:951`, and `gettransaction` first waits for the wallet to reach the tip,
  `src/wallet/rpc/transactions.cpp:755`, `src/wallet/wallet.cpp:1651`); then, when that `gettransaction` still shows no
  confirmation and no conflict, `gettxspendingprevout` with `mempool_only` for the transaction's inputs, a call to the
  node: bitcoind's wallet learns of mempool changes through a queue and can be seconds behind its node, so after a fee
  bump the node has dropped the original while the wallet shows neither a conflict nor the replacement. Another mempool
  transaction that spends the inputs is `errBehind`: nothing is marked and the output is not refreshed; it is in
  `behindInPass`, and the pass's settle counts it as zero if it is not recorded (`views`, with the confirmation bound's
  `boundOut`), and leaves its record standing if it is; the order's other outputs go on as in any pass, its final event
  is held (`heldInPass`: no "no final event" `ALERT` for it) and one `WARN` is logged; when the wallet has caught up,
  `gettransaction` shows the conflict and the order goes the usual way; 60 seconds after the pass first found it
  (`behindSince`, in memory, kept through a return to the mempool and a dead mark, until the process ends; each restart
  grants it once more) the node's word is taken and it is marked dead (`behindGrace`), since the wallet can drop a
  conflict again when the replacement is itself replaced. The transaction itself as the spender (it came back between
  the two calls) is a `mempool` mark; an output whose current mark is `mined` (its block was reorged out) is not asked
  at all and is dead as before; no spender is `dead`. A failed call never marks dead (a transport failure ends the pass,
  anything else skips the output). An output already marked dead is not read again. Then settle the orders touched,
  those past their expiry without a final event, and those awaiting `order.paid_again`; then the gap check. A failed
  `getblockchaininfo`, `getmempoolinfo` or `listsinceblock` ends the pass with one `ALERT` and writes nothing. An output
  is settled at the end of a pass when that pass refreshed it, its order settled without an error, its mark is mined
  `SettledDepth` below the pass's cursor and its forward is recorded: then its mark was verified at a tip no lower than
  the cursor (plan 14 adds a bound: the count is also at most the `confirmations` of the same pass's `gettransaction`
  answer for that block, and zero without one; "The final fixes, as built (plan 14)" below). The fact goes in the store
  (`onchain_settled`, insert-only), and `WatchedTXOs` leaves the output out from then on, in any process, so after a
  start only the unsettled outputs are refreshed. Every other output (dead, unconfirmed, not yet that deep, or not
  refreshed in the pass) is polled on every pass until its order's window closes, and an on-time one past it (the stuck
  set, plan 6.1).
- **Status precedence** (`status`): paid (either rail) > late > underpaid > expired > new. `Rail.Stale` is set before
  the first good pass and when no pass succeeded within 3 poll intervals plus the last good pass's duration, and since
  plan 6.1 for an order in the last complete pass's stale set (its settle failed, or a call for one of its outputs
  did); the page then reads "Checking payment…", and the API's `onchain.stale` is true.
- **Networks** are `mainnet`, `testnet`, `signet` and `regtest`; the address prefix (`bc1`, `tb1`, `bcrt1`) and the
  `xpub`/`tpub` version must match. SatBooth never decodes bech32 (§4); `deriveaddresses` is the cross-check.
- **API fields are named by rail:** `lightning_msat` (as built) and `onchain_sat`, because plan 7 adds a fiat amount
  beside them.
- **The client** speaks JSON-RPC 1.0 with named parameters, basic auth with the password loaded in-process, a
  timeout on every call, an 8 MiB answer cap, and only the twelve whitelisted methods; bitcoind's error texts are dropped
  (they can echo a key), and only the method and code are logged. Amounts are parsed exactly
  (`btcrpc.ParseBTC`), never through `float64`.

### On-chain follow-ups, as built (plan 6.1)

Changes in `onchain`, `store`, `status`, `lnpay`, `limits`, `checkout` and `service`; the rules for the operator and
the merchant are in `docs/bitcoind.md`, the `limits` and `onchain` tables of `docs/configuration.md`,
`docs/merchant-api.md`, `docs/rules-lightning.md` and `docs/rules-onchain.md`. Bitcoin Core claims are marked *source*
(Core 32.1) or
*observed* (`qualify/REPORT.md`, "Plan 6.1 follow-ups").

**Decisions (plan 6.1).**

1. **A dedicated bitcoind.** `docs/bitcoind.md` recommends a bitcoind dedicated to the hub and SatBooth, with no wallet
   that holds keys loaded on it: `rpcwhitelist` is per method, not per wallet (*source*:
   `src/httprpc.cpp:107-118`; observed), and `listdescriptors` with `private` true reaches any unlocked keyed wallet
   (*source*: `src/wallet/rpc/backup.cpp:479-519`). SatBooth needs bitcoind to start (Ruling I stands); the hub needs it
   as its chain source. A quick start with docker compose is plan 10's.
2. **Stuck on-time payments.** An on-time payment still unconfirmed when its order's 7-day window closes:
   `order.underpaid` then, unless the order already has a final event, and the payment keeps being tracked until it is
   mined (then the tier rules, and `order.paid` after `order.underpaid`, Ruling S) or dead. CPFP and acceleration keep
   the txid, so they work; an RBF replacement inherits only by decision 8, and only inside the window (decision 9).
3. **The both-rails `ALERT` when Lightning pays second**, also once the on-chain payment is settled deep. The check
   "`AddEvent(order.paid)` returned false for an order with an on-chain amount" is not used: it is also what `lnpay`
   sees on its own replays (a restart between the event and the release, or `healPaid`), so it would log a false
   "paid on both rails".
4. **Confirmations down to 0.** A tier's `confirmations` may be 0, so a merchant can accept a payment seen in the
   mempool for small amounts; the defaults stay 1/3/6. A replacement or a drop reverts it (`order.reverted`, Ruling
   K). Since Bitcoin Core 28 full RBF is the default (*source*: the Core 28.0 release notes, `mempoolfullrbf` default
   1), so 0 means trusting the payer for that amount (`docs/rules-onchain.md` "0-conf risk", threat model §3). A 0-conf
   record is settled only once mined to the largest tier.
5. **A per-account cap on open invoices.** Each `account_id` may hold at most `max_open_per_account` open Lightning
   invoices: default `min(5, max_open)`, 1 to `max_open`, both classes, on top of `max_open` and the `new` share.
   Lending unused `new` slots to `known` accounts stays. The count is atomic with the creation, under `lnpay`'s mutex;
   the refusal is `limits.ErrAccountCap`, which the page reads as "Busy, try again." (threat model §1).
6. **Per-order staleness.** One order whose settle keeps failing no longer makes every order read stale: `stale` is
   per order, from the set of orders whose settle failed, or whose output's call failed, in the last pass that ran to
   its end. A store query that fails still spoils the whole pass.
7. **Re-read before `dead`.** `getmempoolinfo` is asked again before an output is marked dead, which closes the hole
   of a bitcoind restart inside one pass (§3 above, "The watcher").
8. **An RBF replacement inherits the original's first-seen time**. The rule is per transaction: a replacement inherits
   only when its total to the address equals the total one on-time transaction it replaced paid to it, all its outputs
   to the address or none, and nothing else counted could confirm with it (Rulings 6.1-S and 6.1-U narrow it). Ruling D
   now covers only the other replacements.
9. **Inheritance only inside the watch window.** An on-time ancestor passes on its first-seen time only while its order
   is inside its watch window: the replacement is first listed before `ExpiresAt` + `WatchWindow` (`inherit` reads the
   pass's clock, as for the expiry). Past the window the replacement is a new output with its own first-seen, so it is
   `late` and a person credits it. It closes this option: decision 8 with 6.1-3 (no calendar cap on the stuck set) let a
   payer keep an on-time transaction alive at a fee that never confirms and bump it at the exact amount weeks after the
   expiry, a free option on the quote once prices are in fiat (plan 7). Before it, a bump after the window counted; the
   stuck payment itself is still tracked and counted when it is mined.

**Rulings made during the build.**

- 6.1-1. **The both-rails seam is a func field on `lnpay.Service`** (`SetOnLightningPaid`), set by `service.Run` to
  `(*onchain.Service).LightningPaid`: `lnpay` cannot import `onchain`.
- 6.1-2. **The window-close `order.underpaid` waits for the Lightning side** (`onchain.LightningDone`), like every
  final event, so the API and the events agree. An invoice still open at a 7-day window close is `lnpay`'s
  stale-invoice residual; the order then gets `ALERT: an on-chain order left its watch window without a final event;
  check it by hand`, and its stuck outputs can still settle it later.
- 6.1-3. **No calendar cap on the stuck set**; measure first. (Its interaction with decision 8, a bump that inherits
  weeks later, is closed by decision 9: inheritance stops at the window.) Read in Core 32.1 (*source*): the watch-only
  wallet does rebroadcast a received unconfirmed transaction. `ResubmitWalletTransactions`
  (`src/wallet/wallet.cpp:2147-2180`) submits every wallet transaction that `isUnconfirmed`
  (`src/wallet/transaction.h:388`: not abandoned, not conflicted, not confirmed; nothing about who sent it), through
  `SubmitTxMemoryPoolAndRelay` (`:2050-2092`), every 12 to 36 hours (`:2103-2120`, `:2184-2191`,
  `src/wallet/load.cpp:173`) and at each wallet load without relay (`src/wallet/wallet.cpp:3366`), unless
  `-walletbroadcast=0` (default 1, `src/wallet/wallet.h:133`, `src/wallet/wallet.cpp:3128`; `-blocksonly` sets it to 0,
  `src/wallet/init.cpp:95-96`). A transaction dropped from the mempool becomes inactive and stays unconfirmed
  (`src/wallet/wallet.cpp:152-159`, `:1501-1506`), so it is submitted again. But the mempool drops it `mempoolexpiry`
  hours after it entered (default 336, `src/kernel/mempool_options.h:23`; `src/validation.cpp:278`,
  `src/txmempool.cpp:859-873`), and a rebroadcast while it is there does not reset that time
  (`src/node/transaction.cpp:63-72`; a restart keeps it, `src/node/mempool_persist.cpp:92`, `:103`). A pass that runs in
  the gap marks it dead, and past the window that settles it (6.1-O). So the cost of a stuck output lasts until it is
  mined, replaced or double spent, or until a pass finds it out of the mempool: normally at most about 336 hours after
  it last entered bitcoind's mempool. Not observed. If the cost matters, a cap of 30 days after the expiry is a one-line
  addition to the stuck SQL.
- 6.1-4. **`SettledDepth` is at least 1** (`max(last tier's confirmations, 1)`) rather than a refusal of a last tier
  of 0: an all-zero tier list still settles nothing before a block. `listsinceblock`'s target stays `SettledDepth` + 1.
- 6.1-5. **`confirmations` is a required key of every tier** (`*int` in the configuration): with 0 allowed, an omitted
  key must not read as 0-conf.
- 6.1-6. **A second page sentence for the window-close underpaid**: "Your payment is not yet confirmed enough. The
  merchant will contact you." (the existing one says the payment is below the amount, which is false for a whole
  payment). Two sentences on the page, one state in the API.
- 6.1-7. **The per-account count is `lnpay.Service.byAcct`**, under `lnpay`'s mutex, as `total` is, not a store query
  under that mutex.
- 6.1-8. **The stuck qualification phase runs in its own environment** on a clock shifted by 7 days, last.
- 6.1-9 is not used.
- 6.1-10. **A late output always needs at least 1 confirmation** (`max(tier, 1)`), so Ruling Z2 holds
  on a 0 tier: a person never acts on a late payment the payer can still replace, and a replaced late payment sends
  no `order.late`.
- 6.1-N. **The per-account default is `min(5, max_open)`**, so a `max_open` below 5 with no per-account value still
  starts.
- 6.1-O. **A dead on-time output past the window is settled once its order is final**: the stuck SQL is "first seen
  before the order's expiry and not settled", with no "not dead" clause; past the window a dead output with no standing
  record (never recorded, or every forward reversed, and not foreign) is settled at the end of a pass that refreshed
  it and settled its order without an error, once the order has its final event. That heals a crash between a dead mark and its reversal. Cost:
  store and ledger reads, no extra call.
- 6.1-P. **Inheritance is decided once, at discovery, and stored in `first_seen`.** The residual: a node that heard the
  replacement before the original gives it its own time. A link table refreshed from `walletconflicts` every pass is the
  upgrade if it matters; nothing built is in its way.
- 6.1-Q. **That settle logs `ALERT: an on-time on-chain payment past its watch window left bitcoind's mempool;
  SatBooth stops polling it and will notice it again only if it is mined`**, and a settled dead output that a later
  scan lists with a confirmation is reopened (migration 3, below).
- 6.1-R. **A rebroadcast seen only in the mempool past the window does not reopen a settled dead output**: past the
  window a person is already involved. An order at a 0 tier then waits for a block.
- 6.1-S. **Inheritance needs the exact amount**: with "at least", a small on-time output replaced by a
  larger one after the expiry would make a late payment count as on time.
- 6.1-T. **An inherited first-seen after the clock's now is clamped to now**; `RecordScan`'s refusal of a future time
  stays as the backstop, so a clock step back never fails the scan for every order.
- 6.1-U. **Exclusivity:** a replacement does not inherit when any on-time output of its order, or a
  transaction decided counted earlier in the same scan, is not in its `walletconflicts`. It closes a partial double
  spend before the expiry (more counted than was committed on time). Its costs, both toward late: two independent
  on-time parts with one bumped after the expiry make that part late, and so does cancel, pay again, then bump (the
  cancelled on-time row is still stored, whatever its mark).
- 6.1-V. **An order that waits while bitcoind loads its mempool is not stale**: it reads `seen`, and no
  final event is written in that pass.
- 6.1-W. **One both-rails rule on both rails, checked level-triggered.** The rule (`bothRecorded`): a recorded
  on-chain forward of code 2 or 4, not reversed and not foreign, of any amount, beside a Lightning payment; any money
  on both rails needs a person. `settle` checks it after its records at every call, and `LightningPaid` when Lightning
  pays second.

**Implementation choices.**

- **Event identity.** The window-close underpaid is `EventID("order.underpaid", orderID, 1)`, the same as at expiry, so
  an order gets one of the two; the `order.paid` that may follow it is the usual one, with Ruling S's `ALERT`. No new
  event type. An order that already got `order.expired` and whose on-time payment is live again past the window
  reads `underpaid` with no second final event.
- **`Rail.State` past the window is `underpaid`** when the counted live sum reaches the amount, the order is not paid
  and its `order.underpaid` exists (inside the window, before any unpaid event, it reads `seen`), so `status` derives
  the state the event names. The two final unpaid rail states come from the events: past the expiry, not paid and
  nothing late, with `order.underpaid` or `order.expired` (or an `order.reverted` that no `order.paid_again` answers,
  after which settle writes neither), `underpaid` when a counted sum is live or the event is `order.underpaid`, else
  `expired`; with none, `seen` when a counted sum is live, else `waiting`. The whole amount live before the window end
  no longer reads `seen` ahead of the events: once an unpaid event exists, live on-time money reads `underpaid` until
  `order.paid` (or `order.paid_again`). That is reached without a crash (an on-time payment evicted after a pass's scan
  past the expiry gets `order.expired` a pass later, then is rebroadcast; or a reorg puts a paid order's payment back in
  the mempool past the expiry), and after a crash that lost that in-memory hold and stored an inheriting bump after
  `order.expired`. The events say `expired` while the API says `underpaid` there: the one divergence left, toward the
  safer reading ("Nothing was charged" would be false), until the payment is mined (`order.paid`, Ruling S, with its
  `ALERT`) or dies again (`expired`). `Rail` reads the events once, in its last arm, so for every order past its expiry
  that is not paid and has nothing late; `status.derive` is unchanged. So `expired` lags the expiry by up to one pass, a
  stuck payment that dies after the window's `order.underpaid` reads `underpaid` (the page: "Your payment did not
  confirm."), the pass that holds a final event reads `waiting` or `seen`, and an order whose settle keeps failing past
  its window reads `new` for good, with `logNoFinal` as the signal. The `ALERT` text `logUnderpaidAtWindow` is used only
  for that case; a counted sum below the amount at the window close (a settle that failed all week) gets the expiry's
  `order.underpaid` and `ALERT: an order was underpaid at expiry`.
- **The stuck set is a second arm of two store queries**, `WatchedTXOs` and `HasOpenOnchainOrder`: an output first seen
  before its order's expiry that is not settled, whatever its window or mark. It is a `UNION`, so the first arm keeps
  the `orders_expires` index; the second scans `onchain_txos` once a pass. Measured (the store's own SQLite, one laptop
  core): with 100,000 settled on-time outputs past their window and 5 stuck ones, `WatchedTXOs` takes about 73 ms (7 ms
  at 10,000: linear, about 0.7 µs per output ever stored, almost all of it the `onchain_settled` probe of each row; a
  bare scan of the table is 4 ms), and `HasOpenOnchainOrder`, called only at startup, the same when nothing is stuck. A
  `LEFT JOIN ... IS NULL` form costs the same (67 ms) and no index can bound it, since nearly every row is settled; a
  merge `EXCEPT` of the two keys halves it (30 ms), still linear. A bounded form needs the unsettled outputs held apart
  (a table with a guarded delete, or an in-memory set seeded at startup), which would be a design change; the query is
  unchanged. `HasOpenOnchainOrder` keeps the startup guard (no start without the `onchain` block) until every stuck
  output is settled, dead ones included. `ALERT: an on-chain payment left its watch window unconfirmed` and its code are
  gone. After an upgrade from a plan 6 build, the first pass polls again every on-time output that was never settled,
  whatever its window.
- **Migration 3** (Ruling 6.1-Q needs it). It narrows `onchain_settled`'s delete guard: a settled
  fact may be deleted only while its output's current mark is `dead`. `RecordScan` deletes it, in the scan's
  transaction, when the scan lists that output with a confirmation (the sender rebroadcast it and it was mined); the
  output is then tracked and recorded by the usual rules. Updates and replaces stay refused, and the marks keep the
  history.
- **The stuck set's bound and cost.** Bounded to on-time outputs not settled: each transaction costs one `gettransaction` plus one
  `getmempoolentry` a pass, shared by its outputs and paced at `poll_rate`, until it is mined `SettledDepth` deep with
  its record, or dead and settled (6.1-O); the pass that marks it dead costs one more `getmempoolinfo`, `getmempoolentry` and
  `gettransaction`, and a `gettxspendingprevout` (six calls in that pass; the same each pass while the wallet is
  behind); an output already dead costs what a live one costs (two calls). A late
  output of a touched past-window order, in the mempool or mined and not recorded, costs the same while that order is
  settled (plan 14: it is read again before it is recorded). A mined output whose ledger record is foreign
  never settles and is polled until it is put right by hand. How long bitcoind keeps a stuck transaction: 6.1-3.
- **The both-rails seam and the watcher's half.** `completePaid` calls the hook after `order.paid` for an order with
  an on-chain amount; a hook error leaves the invoice not done (it keeps its slot), so the next lookup completes it
  again, hook included. `settle` keeps its level-triggered check after its records (6.1-W) and, when it created a
  record while Lightning read unpaid, reads Lightning's status once more: each rail writes its record and then reads
  the other's, so at least one sees both. One `ALERT` per order per process (`bothAlerted`, bounded at 4,096 and
  cleared when full). Residuals: it repeats once after each restart; it can precede a reversal in the same settle;
  a late payment on a Lightning-paid order logs the late `ALERT` too; it is lost when the on-chain record is settled
  deep and Lightning's completion does not reach the hook (`healPaid` whose hook call fails, or `settleOrder`'s heal
  of a closed invoice, which does not call it).
- **0-conf by construction.** A `mempool` mark is written only when `getmempoolentry` found the transaction, or
  `gettxspendingprevout` named the transaction itself as the spender of its inputs, so only once the mempool holds it;
  `loaded` gates `dead` marks, never `mempool` ones. "Counted from the mempool" is: the output's current mark is
  `mempool`, it is counted, and its need is 0. An output not yet marked is not recorded on any tier. A 0-conf record is
  reversed only when its output is dead; back in the mempool after a reorg it stays recorded.
- **Late never from the mempool** (6.1-10): `need` is `max(tier, 1)` for an output that is not counted.
- **The per-account count, `byAcct`,** counts the account's slot-holding open invoices, as `total` does for
  `max_open`: a paid or stale invoice holds none. It is derived from the store's open invoices at start and kept by
  `reserve`, `release`, `adopt` and `detach`. `lnpay`'s mutex is not held across a per-request store call: the daily
  figures are read under it once a day (`AttemptsSince`).
- **The per-account cap is check 7 of `limits.Admit`**, after the class share and before the token, so a refused
  request burns no token, and a `new` account over its own cap reads the share's refusal first when the share is full.
- **A skipped order is stale too**: the per-order set holds the orders whose settle failed and those an output's
  failed call skipped (6.1-V: not those skipped while the mempool loads). A pass that ends early leaves the previous
  set.
- **Inheritance at discovery** (`onchain.inherit`): groups are one transaction's kept outputs to one address; a group
  with a stored row keeps its rows; a group of an order whose watch window has closed at the pass's clock inherits
  nothing (decision 9); a chain of bumps lists every ancestor (`bumpfee` keeps the inputs), and a stored
  middle bump already carries the first one's time. `walletconflicts` lists every wallet transaction that spends one
  of the same inputs, whatever its state (*source*: `src/wallet/wallet.cpp:695-715`, `:2093-2100`,
  `src/wallet/rpc/transactions.cpp:47-50`; the RPC help's "Confirmed transactions" is narrower than the code;
  observed for a bump: `qualify/REPORT.md`, "Plan 6.1 follow-ups"). The invariant, tested by a seeded property test: a set
  of outputs that can confirm together never counts more than was committed on time. The dead original of an
  inheriting bump settles past the window without its `ALERT` (`carriedOn`, matched on the same first-seen second:
  an unrelated payment of the order first seen in the same second can also silence it). A known residual: the
  exact match needs the inheritance link stored, which is a migration, and the `ALERT` is all it costs.
- **The stuck qualification phase** runs in its own environment (its own data directory, the same watch-only wallet)
  on a clock shifted by 7 days, the only way to see a window close on regtest; it runs last, because it issues
  addresses above the shared environment's record. A stated departure from plan 6's "one environment".

### Fiat pricing, as built (plan 7)

**Built (plan 7).** The code is `rates` (the decimal, the
conversion, the providers and the fetcher), `store` (the `order_quotes` table), `api`, `webhook`, `checkout` and
`service` (the `rates` block); `docs/configuration.md` and `docs/rules-fiat.md` have the operator's view ("The `rates`
block", "Rules for fiat pricing"). Its threat model is §10 of [`threat-model.md`](threat-model.md). Vendor claims are
marked *source* (the vendor's documentation) or *observed* (an unauthenticated request on 2026-11-05).

1. **SatBooth converts fiat prices**, the way BTCPay does. Earlier, §1 said SatBooth does no currency math. Built.
2. **Providers:** Kraken, Coinbase and Yadio, plus a custom rate API (an operator-configured URL that returns a JSON
   format SatBooth defines; it goes through the same median, staleness and disagreement checks). Bitstamp (a signed data
   licence for commercial use) and CoinGecko (a credit, no redistribution, limited keyless use) are not providers
   because of their terms. `ACKNOWLEDGEMENTS.md` credits Yadio, as its terms ask. The operator picks which ones to use
   (`rates.providers`, and `rates.custom_url` or, for a URL that carries a token, `rates.custom_url_file`, a 0600 file).
   Built.
3. **Margin:** one percentage per store, applied after the median. It may be negative (a discount). No rule language.
   Built (`rates.margin_percent`, -50 to 50).
4. **Rate safety:** the median of at least 2 providers; refuse to price an order when the providers differ by more
   than 2%, or when a rate is older than 5 minutes (both bounds configurable); the rate is locked until the order's
   expiry and recorded with the order: the rate, its sources and the margin. Built (`min_sources`,
   `max_spread_percent`, `max_age_seconds`, `order_quotes`).
5. **Modes:** keep both. An order carries either a fiat price and currency, or explicit per-rail amounts, never both.
   Today's explicit-amount API is unchanged. Built (`mixed_pricing` refuses both).
6. **The threat-model section on rate sources comes first**: the rate is a money input, and the providers are new
   outbound destinations from the public host. Done (threat model §10, written before the code, marked as built).
7. **Monero (plan 8) gets its XMR rate from the start:** design for a rate per asset, build only BTC. Built as below
   ("Per asset").
8. **The Yadio credit:** the checkout page shows a small "Rates: yadio.io" link only when Yadio was
   among that order's quote sources, and `docs/configuration.md` tells the operator that the same applies wherever else
   they show the data (threat model §10, threat 7). Built: the page ends with `Rates: <a
   href="https://yadio.io">yadio.io</a>` (only `yadio.io` is the link), and `docs/configuration.md`'s "Credits and
   terms" says the rest.

**Implementation choices.**

- **A background fetcher with a cache** (BTCPay's model), not a fetch at order creation: one request per provider per
  asset every `fetch_seconds` (default 60), all providers in parallel and every rate of a pass stamped with the
  clock at its start, whatever the order rate; the quote never calls the network. A provider's good answer replaces
  its cached rates as it arrives; a failed one keeps the old ones, which age out. Fiat orders are refused with 503
  `rate_unavailable` (with `Retry-After: 10`) until the first fetch has filled the cache, and whenever the rules fail;
  explicit orders are untouched. The service starts whether or not a provider answers. One call is bounded by the
  client's 10 s timeout and, should a provider not honour it, by 11 s.
- **The currencies are an operator list** (`rates.currencies`, 1 to 32 codes),
  not "whatever the providers quote": Coinbase's `/v2/exchange-rates` returns 661 codes, crypto included
  (*observed*). Each provider is asked for, or filtered to, the configured codes; the API validates `currency`
  against the list (`invalid_currency`); a configured code that fewer than `min_sources` providers quote is
  `rate_unavailable` at order time. Parallel-market currencies such as ARS and VES may never price: the providers
  can differ there by more than the 2% bound (threat model §10, threat 1). **Ruling 7-R (startup):** the one
  currency refusal at startup is a code outside Kraken's seven (AUD, CAD, CHF, EUR, GBP, JPY, USD) when Kraken is
  configured and the other sources number fewer than `min_sources`, so the code could never reach it. Coinbase, Yadio
  and the custom API are assumed to quote any code, since that is known only after a fetch. `service` reads Kraken's
  list from `rates.KrakenFiats` (a copy of the one list). **Changed by plan 8:** the rule counts Bitfinex's BTC list
  too (`rates.Quotes`, `rates.BitfinexFiats`: EUR, GBP, USD), and the text names the table providers configured, for
  BTC: `config: rates.currencies has a code that fewer than min_sources providers quote for BTC (kraken quotes only
  AUD, CAD, CHF, EUR, GBP, JPY and USD; bitfinex quotes only EUR, GBP and USD)`. XMR has no startup rule (ruling 8-A,
  "Monero payments, as built").
- **A fiat order gets every configured rail by default** (Lightning, and on-chain when the `onchain` block exists),
  the same number of sats on each; an optional `rails` array narrows it. A priced amount outside a selected rail's
  bounds is refused with that rail's existing code (`invalid_lightning_msat`, `invalid_onchain_sat`), never clamped,
  and no rail is dropped; `"rails":["onchain"]` without the `onchain` block is `onchain_disabled`.
- **Rounding:** the sat amount is the ceiling of `price × 10^8 × (1 + margin) / rate`, computed on big integers as
  `ceil(P × 10^8 × (10000 + margin_bps) / (R × 10000))` over the units of price and rate; Lightning gets
  `sats × 1000` msat, on-chain gets `sats`. The ceiling never under-charges the merchant; the customer pays at most
  1 sat over. One function, `rates.Sats`, used by the API alone. No floating point in an amount, a rate or a margin:
  a source scan (`rates.TestNoFloatInMoneyCode`) keeps float types, float literals and float conversions out of
  `rates`, `api`, `store` and `checkout`, and `rates.Decimal` (an exact integer of units at scale 10^18) cannot be
  compared with `==`.
- **The median of an even count is the lower mean** (`floor((a + b) / 2)` in units); the spread rule is
  `(max − min) × 10000 > max_spread_bps × min`; a rate with no timestamp (Kraken, Coinbase) is stamped with the fetch
  time; a rate stamped more than 60 s in the future is dropped, as is one older than `max_age_seconds`. Two fresh
  rates from one provider are refused (`ErrDuplicate`); the cache keeps one entry per provider name, and
  `rates.New` refuses two providers with one name. Kraken's rate is the mid of its best bid and ask, kept only when
  0 < bid ≤ ask (as BTCPay's `KrakenExchangeRateProvider` keeps a pair); a Coinbase value with more than 18 fraction
  digits drops that code rather than being rounded.
- **One lying provider among three blocks pricing** (the spread rule fails) rather than being voted out: fail closed,
  no outlier dropping, and an `ALERT` names the providers. As built, `ALERT:
  fiat pricing is unavailable for a currency` carries `asset`, `currency`, `reason` (`too_few` or `disagree`),
  `providers` (the fresh ones) and, for `disagree`, `low` and `high`; it is logged on a change of state among
  `available`, `too_few` and `disagree` (the first pass included), and `fiat pricing is available for a currency`
  marks the recovery. A failed request logs `rate fetch failed` with the provider, an error class and the HTTP status:
  a warning for the first failure of a provider's run of failures, debug after that until the provider answers again.
  No line carries a rate, a URL or a provider's text. There is no hysteresis: a provider that hovers at the age bound
  can make the `ALERT` come and go.
- **`min_sources` is 2 to 4** (plan 7, decision 4: the median of at least 2 providers), so the custom API never prices
  alone.
- **Percentages are decimal strings in the configuration** (`"margin_percent": "1.5"`, `"max_spread_percent": "2"`),
  held as basis points in the code and the store; a JSON number there is refused (the strict parser's generic text).
  The configuration's defaults: `margin_percent` `"0"`, `min_sources` 2, `max_spread_percent` `"2"`,
  `max_age_seconds` 300 (30 to 3600), `fetch_seconds` 60 (10 to 600, at most half of `max_age_seconds`). The custom
  URL is `https` with a host and no user info or fragment, and no query (a URL with a query, a token, goes in
  `rates.custom_url_file`). `service.Run` validates the block again, so a configuration built in code without
  `ParseConfig` cannot start a fetcher that prices from one source or with an unchecked margin (`startup failed`,
  reason `rates`).
- **The quote is its own immutable table** (`order_quotes`, migration 4, one row per priced order, written in the
  order's transaction), not columns on `orders`. The `price` is stored as the merchant sent it (validated), so the
  page shows `12.50`, not `12.5`. The sources are a JSON array of `{provider, rate, at}`, at most 8; times are whole
  Unix seconds. A quote that does not read back as valid is an error, never a silent default.
- **Idempotency by `merchant_ref` compares the pricing, not the amounts:** a repeat of a priced order with the same
  account, `price` (byte for byte), `currency` and rail set returns the existing order with its locked rate; a priced
  repeat of an explicit order, or the reverse, is 409 `conflict`. A repeat returns the existing order: the API looks the
  reference up before any conversion (plain reads; the store's transactional check stays the arbiter when two first
  requests race), so a repeat is 200 with the original order during a rate outage, after fiat pricing or the currency
  was switched off, and when today's rate would put a rail out of bounds. A merchant who lost a response retries the
  same `POST` (they have no id to `GET`). A request under a reference that another order holds (another account,
  price text, currency or rail set, or an explicit order) is 409 `conflict` from that same lookup, before any
  pricing, so also during an outage or with fiat pricing off; the lookup and the store's transactional check call one
  rule (`store.PricedRepeat`, `repeatsPriced`). A request under an unused reference is priced as usual and meets the
  store's check. A repeat without `rails` after the operator changed the rails is 409.
- **The API's `pricing` member** is on every response, `null` for an explicit order (the house style for an absent
  rail), with the times in RFC 3339.
- **The webhook body gains `pricing` only for a priced order** (`omitempty`, times in Unix seconds), so an explicit
  order's body is byte-identical to the plan 6 one and the HMAC vector of `docs/webhooks.md` stands. A quote that cannot
  be read holds the event back (retried), so an event is never sent without its price: a store read failure ends the
  pass, a malformed stored quote (`store.ErrStoredQuote`) holds back that event alone (it stays pending) and the pass
  goes on. `ALERT: a webhook event is held back: its order's stored quote is malformed` names the event once per event
  per process (a set bounded at 4,096 and cleared when full, as `bothAlerted`); the later passes log at debug level,
  without `ALERT`.
- **The checkout page** shows the price and currency above the sats and BTC amounts. The page shown while the
  on-chain rail is not configured for an on-chain order ("Checking payment…") shows neither the amounts nor the
  price, as before plan 7.
- **A test seam for the fixed hosts:** `service.WithRateTransport` (tests only, as `WithWallet` and `WithRPC`) and
  `rates.NewClient`; the configuration stays strict (HTTPS, the providers' fixed hosts), and the fake rate server
  (`rates/ratestest`) is reached by a round tripper that re-routes those hosts (the custom URL's host in tests is
  `rates.example.invalid`).
- **Recorded live responses are the parser fixtures** (`rates/testdata/`), written by `TestLiveRecord` (build tag
  `live`), which keeps only what the parser tests read (Kraken's seven pairs; Coinbase's seven codes plus the code with
  the longest fraction of the answer when that is longer, so little of the
  vendors' data is redistributed) and runs only by name and with `SATBOOTH_RECORD_FIXTURES=1`, so the optional live smoke test in the same
  file (`TestLiveSmoke`) never rewrites them. `TestFixturesAgree` checks offline that the three recordings agree
  within 2% in each of Kraken's seven currencies.
- **The outbound client** is the standard library's: HTTPS only (also refused inside the request function), TLS 1.2
  or later, no redirect followed, no proxy from the environment, a 10 s timeout, at most 256 KiB of a body, 8 KiB of
  headers, idle connections closed after 30 s, and `User-Agent: SatBooth`. A provider's error comes back as a class
  (`transport`, `status`, `too_large`, `shape`, `vendor`), never with Go's transport text, which holds the URL.
- **Capabilities.** `rates` and `rates/ratestest` add NETWORK to `docs/capslock.json`. `rates` also gains the set
  every package that logs through `*slog.Logger` carries (ARBITRARY_EXECUTION, FILES, SYSTEM_CALLS, UNSAFE_POINTER)
  and RUNTIME from `context.WithTimeout` (accepted); no other package changed.

**Per asset.** The rate is designed per asset, and only BTC is built. A provider fetches the rates of one asset
against a list of currencies (`Provider.Fetch(ctx, asset, currencies, at)`), the cache holds each provider's last
rates per asset, `rates.Config.Assets` lists the assets (`["BTC"]` today), and a quote is per asset and
currency; `rates.Sats` is the one conversion for any asset with 10^8 base units. A provider that does not quote an
asset answers `ErrAsset`, an empty answer rather than a failure. Plan 8 adds for XMR: a provider that quotes XMR,
`Assets` gaining `"XMR"`, and the API's asset per rail (`rails` naming `monero`, priced from the XMR quote); since
`rates.Sats` is written for 10^8 base units, the conversion to piconero (10^12 per XMR) needs its own base as well. The custom rate
API serves one asset per URL; for a second asset SatBooth appends `?asset=<A>` (plan 8, decision 10, below). **Built
by plan 8:** Kraken
and Bitfinex quote XMR, `Assets` gains `XMR` when the `monero` block is set, `rates.Convert` serves both bases
(`Sats`, `Piconero`), and the quote is recorded per asset (below).

### Monero payments, as built (plan 8)

**Built (plan 8).** The code is `rail` (the rules the on-chain and Monero rails
share), `xmrrpc` (the wallet-rpc client with HTTP Digest), `monero` (subaddresses, the startup checks and the watcher;
`monero/monerotest` is the in-memory wallet-rpc of the tests), `store` (the Monero tables, migration 5, and the quote
per asset), `rates` (Bitfinex, the XMR pairs, `Convert`), `status`, `api`, `webhook`, `checkout` (the rail picker) and
`service` (the `monero` block). `docs/configuration.md`, `docs/monero.md` and `docs/rules-monero.md` have the operator's
view ("The `monero` block", "Setting up monerod and
monero-wallet-rpc", "Rules for Monero payments"). Its threat model was written before the code and is marked as built:
[`threat-model.md`](threat-model.md) §0 (the wallet-rpc login), §3 (what makes a Monero order paid), §5, §8, §9 and
§10. Upstream facts are marked *source* (monero-project/monero tag v0.18.5.4, `path:line`), *observed* (an
unauthenticated request on 2026-11-07, or the regtest run, `qualify/REPORT.md` "Monero end to end") or *inferred* (a
reading of the source). Every decision below is built.

1. **wallet-rpc confinement:** full RPC, reachable only over the private link, HTTP Digest login whose password SatBooth
   loads from a 0600 file (never argv, logs or errors). wallet-rpc has no per-method whitelist, and its restricted mode
   cannot issue a subaddress or show the tx pool (*source*: `src/wallet/wallet_rpc_server.cpp:142`, `:761-763`,
   `:2906-2910`). The threat model states that the login's holder can fake Monero payments (`set_daemon`) and stop or
   rescan the wallet, inside §0's bound.
2. **Placement:** monerod and wallet-rpc on a third private host if the operator can afford it, else on the private
   hub host; never on the public SatBooth host. Running them is the operator's job; `docs/monero.md` states it. Since
   plan 10 the hub bundle's `monero` profile runs them on the hub host ("Operations, as built (plan 10)").
3. **Wallet:** the operator creates the view-only wallet (`monero-wallet-cli --generate-from-view-key` or
   `--generate-from-json`, with a restore height); wallet-rpc runs with `--wallet-file`. SatBooth takes no Monero
   key: only the expected primary address, checked at startup, plus a startup check that the wallet is view-only
   that never asks for a spend key (`sign_transfer` with an empty set, expecting -29, *source*:
   `wallet_rpc_server.cpp:1382-1387`; *observed*: -29 on the view-only wallet, -39 on a full one). `docs/monero.md`
   advises a fresh wallet used only for shop income. This replaces §2.3's `generate_from_keys`.
4. **XMR rates:** Bitfinex is added as a provider (XMR/USD, and BTC where it is trivial), so XMR prices in USD from
   Kraken and Bitfinex under the unchanged rules of the threat model's §10; Kraken with the custom API also works.
   Coinbase and Yadio do not
   quote XMR, and among fiat currencies Kraken quotes it in USD and EUR only (*observed*, 2026-11-07). The
   decision's "other currencies answer `rate_unavailable` for XMR" is replaced by decision 12.
5. **Default rails:** a fiat order gets Monero only when `rails` names it; the default set stays Lightning and
   on-chain, so a bitcoin sale never depends on the weaker XMR sources.
6. **Confirmations:** tiers by piconero amount, defaults 1 / 3 / 10; 0-conf allowed per tier (plan 6.1, decision 4),
   reverted when `double_spend_seen`. A payment with a non-zero `unlock_time` beyond the normal 10-block lock is never
   counted: it is treated like a late payment (recorded, `ALERT`, a person decides).
7. **Subaddresses:** one per order, allocated on the "pay" press, never reused, never minor index 0; an `ALERT` past
   a configurable run of unused subaddresses; `docs/monero.md` tells the merchant to raise their wallet's lookahead.
   Plan 6's crash protocol carries over (record the index before showing the address, `store` after `create_address`,
   each new index above every recorded one).
8. **Checkout:** a rail picker without JavaScript, "Bitcoin" or "Monero", each with its own QR; the subaddress is
   allocated when its "pay" is pressed; once money is seen on one rail the page stops offering the other. The Monero
   URI carries `tx_amount` only. The both-rails `ALERT` covers Monero beside either bitcoin rail.
9. **Late, partial, over-payment:** plan 6 and 6.1's rules carry over unchanged, minus the RBF first-seen
   inheritance.
10. **Custom rate API:** SatBooth appends `?asset=<A>` (or `&asset=<A>` when the URL has a query).
11. **Qualification:** monerod and wallet-rpc built from the official release tarball, SHA-256 pinned from the
    PGP-signed `hashes.txt`, on the repo's digest-pinned Debian base; v0.18.5.4 (not qualified against v0.19). Built:
    `dev/regtest/monero/`, sixteen phases in `qualify/monero_test.go`; wallet-rpc answered version 1.32.
12. **Rate reachability (ruling 8-A):** a pair the configured providers can never reach
    (fewer than `min_sources` of them quote that asset in that currency, the custom API counting as yes) is not
    fetched and raises no `ALERT`; a priced order naming `monero` in such a currency is a permanent 400
    `invalid_currency`, so no client retries it forever; 503 `rate_unavailable` stays for an outage of a reachable
    pair.
13. **The piconero bound:** every Monero amount is 1 to 2^53 − 1 piconero (9,007,199,254,740,991, about 9,007 XMR),
    one bound in `rates`, `store`, the SQL CHECK, the API and the conversion, so every amount an API client parses
    stays exact in a JavaScript double, as `onchain_sat` does.
14. **`tx_amount` keeps its exact decimal form**: the Monero URI writes the
    piconero as an exact decimal of up to 12 places, without trailing zeros (`xmrrpc.FormatXMR`). Under decision 13's
    bound every amount is below 2^53 piconero, so a sending wallet that parses the decimal as a double still recovers
    it exactly, and the one-piconero-short case never arises.

**Implementation choices.**

- **A `rail` package holds every rule two rails share**, moved out of `onchain` with no behaviour change (`onchain`'s
  own tests were the proof): the tiers (`rail.Tier`, `Required`, `SettledDepth`, `ValidTiers`), the payment predicates
  (`Out.Live`, `Need`, `Recordable`, `Reverts`), the sums, the reversal numbering and plan (`PlanReversals`), the
  final-event rule (`Final`), `LightningDone`, the rail view (`rail.View`) and its state. Plan 8 also moved more, so
  Monero's settle copies nothing: the reversal healing, the paid-before and overpaid checks, the events fold
  (`EventsOf`), the crash-safe order of the paid events (`PaidEvents`, behind the `Paying` interface) and the walk over
  a payment's ledger epochs (`Book`). What stays in `onchain` is Bitcoin's own: RBF inheritance, the dead-after-scan
  hold, the mempool-loaded gate, the cursor's block hash; the epoch loop and the write order stay per rail, kept in
  agreement by each rail's crash and replay tests. Not built: one generic settle over an abstract store.
- **Package layout:** `rail` (shared rules), `xmrrpc` (the Digest JSON-RPC client), `monero` (the rail),
  `monero/monerotest` (the in-memory wallet-rpc), mirroring `btcrpc` / `onchain` / `onchaintest`.
- **The wallet-rpc client** speaks JSON-RPC 2.0 to `/json_rpc` with named parameters, HTTP Digest (MD5, `qop=auth`, as
  epee's `http_server_auth` offers it) with the password held in-process, a timeout, no redirect, no proxy from the
  environment and at most 8 MiB of a body read. It sends only `get_version`, `get_height`, `get_address`,
  `create_address`, `store`, `get_transfers`, `get_transfer_by_txid` and `sign_transfer` (the last only with an empty
  `unsigned_txset`, as the view-only probe), and refuses any other method before sending it. It refuses a wallet-rpc
  that answers without asking for a login, on every call. It is built with `xmrrpc.New(cfg, password)`, and the password
  sits two pointers away from the `Client`, so no formatting of a client or its configuration prints it (`btcrpc` got
  the same constructor).
- **The quote record is normalised per asset:** `order_quotes` keeps the price, currency, margin and time;
  `order_quote_rates` holds one row per asset (`BTC`, `XMR`) with its rate and sources. The API's `pricing` object
  became `{price, currency, margin_percent, quoted_at, rates: {"BTC": {rate, sources}, "XMR": {…}}}` and the webhook
  body the same (Unix seconds). Nothing was deployed, so plan 7's shape was changed rather than a second shape added.
  The store refuses a quote whose assets differ from the order's rails (BTC exactly when a bitcoin amount is set, XMR
  exactly when a Monero amount is).
- **Migration 1 and migration 4 are edited in place** (plan 6's rule: a migration is edited only while no release
  exists): `orders` gains `amount_piconero` and its CHECK; `order_quotes` loses `rate` and `sources` to
  `order_quote_rates`. Migration 5 adds the Monero tables (subaddresses, payments keyed by (`txid`, subaddress) with
  `first_seen_height`, marks, the cursor, the settled set), insert-only and guarded by triggers as plan 6's. `store.Open`
  refuses a file from before this plan with a fixed text, and a version-5 file whose cursor has no `wallet_height`
  (an early build of this plan, before 8-A2 edited migration 5 in place) or whose marks have no `confirmations`
  (before 8-A6 did) with another (`ErrOldMoneroSchema`, R5).
- **One conversion per base unit:** `rates.Convert(asset, price, rate, marginBps)` dispatches to one unexported
  formula parametrised by the scale (10^8 for `BTC`, 10^12 for `XMR`) and the bound; `Sats` stays as the BTC
  wrapper, `Piconero` is the XMR one. `MaxPiconero` is decision 13's bound, the piconero analogue of `MaxSats`;
  `store.MaxPiconero` equals it, with an agreement test. Nothing in `limits` changes: the Lightning limiter is
  msat-only, and the per-order bound of a rail lives where `MaxSat` does. `rates.TestNoFloatInMoneyCode` now scans
  `rail`, `xmrrpc` and `monero` too, so `monero.Config` holds no `float64`: `service` turns `poll_rate` into the
  pause `PollEvery = time.Second / rate`, the one place that holds the float.
- **Marks are the same three kinds** (`mempool` for the tx pool, `mined` at a height, `dead`), so the shared predicates
  apply. A Monero `mined` mark carries a height only (wallet-rpc reports no block hash); a reorg is seen when the
  payment's refresh says it is no longer mined, as for bitcoin. A pool entry with `double_spend_seen` is `dead` only on
  a 0-conf tier (decision 6: the revert is for 0-conf); on a tier that needs a block it stays `mempool` with one `ALERT`
  per payment per process, since the block decides. An empty answer of `get_transfer_by_txid` (wallet-rpc does not know
  the transaction) is `dead`, except while the wallet is below the stored cursor (catching up), when it marks nothing.
  `Payment.Confirmed` (read, never stored) is true for an entry listed `in`: a dead settled payment is reopened only
  when a scan lists it mined, so a re-relay to the pool does not reopen it.
- **A payment is keyed by `(txid, subaddress)`**: wallet-rpc aggregates the outputs of one transaction to one
  subaddress into one `in` entry (*source*: `src/wallet/wallet2.cpp`, `payment_details`), so that pair is the
  natural output; one transaction paying two orders is two rows, and the tracker reads `get_transfer_by_txid`'s
  `transfers` list, not its first entry (*observed*: one transaction to two subaddresses lists both).
- **`counted` for Monero is "first seen before the expiry and not locked beyond the normal lock at first sight"**: each
  payment row records the wallet's height when it was discovered (`first_seen_height`), and an entry with
  `unlock_time > 0` and `unlock_time > first_seen_height + 10` (a height form; a timestamp form is at least
  `CRYPTONOTE_MAX_BLOCK_NUMBER`, 500,000,000, *source*: `src/cryptonote_config.h:40`, read as a timestamp by
  `is_tx_spendtime_unlocked`, `src/wallet/wallet2.cpp:7451`, and so always exceeds it) is late for good, whatever its
  first-seen time. One predicate, fixed per row, used by discover, settle and the view: a classification never flips
  as the chain grows.
- **The combined state over three rails:** each rail's settle writes the final event only when the other rails are
  done (Lightning done as today, and no peer rail *pending*: a peer whose counted live sum reaches its amount and is
  not yet paid, the one peer state that can still turn paid after the expiry); a partial peer makes the final event
  `order.underpaid`. `rail.Peer` is the probe, and `service` wires `onchain` and `monero` as each other's peer. The
  both-rails `ALERT` fires on a record when any other rail reads paid **or holds money** (received, late or a
  record): rule 6.1-W, money on two rails needs a person (`rail.BothRails`, the one definition). After a record each
  rail re-reads Lightning and the peer. The store refuses a second final event of another type (`order.expired`
  against an existing `order.underpaid` and the reverse) inside its write transaction, so two settles racing cannot
  store contradicting events. Lightning paying second tells every watched rail (one fan-out in `service`).
- **Digest calls are serialised:** `xmrrpc` holds one request in flight. epee keeps the Digest state (the nonce and
  the request counter) per connection (*source*: `http_server_auth m_auth` in the connection's handler,
  `contrib/epee/include/net/http_protocol_handler.h:190` and `:227`, the per-connection `m_handler` of
  `abstract_tcp_server2.h:271`), while the client keeps one Digest state for its pooled connections; one call at a
  time keeps its `nc` in step with the session whose challenge it holds, and a call that lands on another connection
  gets a 401 and its one retry. *Observed*: 20 concurrent `get_height` calls through one client all succeeded; two
  Digest clients against one wallet-rpc were not tried. The checkout press still answers `ErrBusy` at `PressWait`
  (10 s).
- **Monero's ledger** is ledger 3 (piconero), accounts `MoneroWallet` (`0x584d_0001`, code 5) and `MerchantMonero`
  (`0x584d_0002`, code 6), transfer codes 2 / 3 / 4 as on-chain, id domains `satbooth/monero-paid/v1` and
  `satbooth/monero-reversed/v1`.
- **The unused-run alert defaults to 100** (range 1 to 10,000): a wallet's lookahead is 200 minor indices past the
  last index it saw used (*source*: `src/wallet/wallet2.cpp:128-129`), and the alert should sound with half of it
  left, not at the edge; the advice in `docs/monero.md` to raise the lookahead (`set subaddress-lookahead`) goes with
  it.
- **The checkout press names its rail** with a form field (`rail=bitcoin` or `rail=monero`) on the existing `POST
  /c/{token}/pay`; absent, it means bitcoin when the order has a BTC rail, else monero; a field that is not exactly
  one of the two, or names a rail the order lacks, is a 400. Each rail's "pay" button is its own form; the page shows
  the pressed rail's fields and, while nothing is seen, still the other's button. A Monero press runs only the Monero
  rail.
- **The wallet's forgotten subaddresses are re-created at startup**: if the wallet does not know the recorded maximum
  index (-15 for it: a `SIGKILL` before `store`, or a restored wallet file), the startup check calls `create_address`
  once per index until the wallet's last index reaches it (one call per index, though `create_address` takes a count: a
  rare path not worth a wider client), verifies that the address at the recorded maximum is the recorded one, and stores
  the wallet; a mismatch refuses to start. *Observed* on regtest: a restored wallet re-created indices 15 to 17 (`count`
  3), the subaddress at 17 equalled the stored one, the next orders got 18 to 20, and a payment to a re-created
  subaddress was found.
- **Pacing, staleness and the per-order stale set** are as on-chain: `poll_seconds`, `poll_rate`,
  `timeout_seconds`, `stale` after 3 poll intervals plus the last pass's duration. While the wallet is below the
  stored cursor every order a pass touches is stale.
- **Networks:** `mainnet`, `testnet`, `stagenet`, `regtest`; regtest uses mainnet-format addresses (*source*:
  `tests/functional_tests/functional_tests_rpc.py:51-65`). SatBooth never decodes base58: it checks the length (95),
  the charset and the first character (testnet's primary address starts with `9` or `A`); `get_address` is the
  cross-check.

**Rulings while building (plan 8).**

- **8-A (decision 12):** an unreachable pair is not aggregated, cached for quoting or alerted; an asset with no
  reachable currency is not requested; a provider's single request may still name an unreachable code, and that code's
  answer is dropped. A custom URL that already carries `asset=` is refused at validation.
- **8-T6a, 8-T6d: the startup check names its causes.** Its errors are fixed sentinels: -13 at any step is `ErrNoWallet`
  (`monero: wallet-rpc has no wallet open; start it with --wallet-file`), a refused login `ErrLogin`, -14 on the
  base-address step `ErrAccount` (`monero: the wallet has no account account_index`), -7 from the probe `ErrRestricted`,
  any probe code but -29 `ErrNotViewOnly`, then `ErrAddress`, `ErrVersion`, `ErrHeight`, `ErrIndexBehind` and
  `ErrSubaddress`; any other failed call is `ErrUnavailable` with the step number (1 the version, 2 the base address, 3
  the probe, 4 the height, 5 the subaddress record; `monero.CheckStep`). Only these fixed texts reach a log or an error.
  Step 2 asks `get_address` for index 0 alone: an empty list makes wallet-rpc list every subaddress of the account,
  which grows past the 8 MiB cap.
- **8-T7a: the watcher re-runs the check** while the wallet is behind on subaddresses or no check has passed; a failed
  check clears the base address, so nothing is issued until one passes. Every other pass first asks `get_address` for
  the account's index 0 and compares it with the base address the last passed check kept; on a difference it clears the
  base, pauses as 8-T7c and ends without writing, so the next pass re-runs the check. A wallet replaced mid-run would
  otherwise answer "not found" for every tracked txid and send false `order.reverted`. The cost: one call per pass
  (three with 8-A6's end check below, which repeats this read and `get_height` after the pass's last call). A press that
  needs a new subaddress makes the same read, under the allocation lock and before `create_address`; on a difference it
  pauses the same way and the press gets an error that wraps `ErrUnavailable` (`?busy=1`, 8-T8a), so the wrong wallet's
  subaddress is never shown. The cost: one call per new subaddress. **With 8-A3:** the step-3 `get_address` that
  cross-checks the new subaddress also carries the account's base address, which must equal the one the identity read
  checked; a wallet swapped between that read and `create_address` answers both later calls consistently and is caught
  only there. On a difference the press pauses the same way and stores nothing.
- **8-T7b: confirmations are wallet-rpc's own count**, `get_height − height` (*source*: `get_height` answers the
  block count, `wallet_rpc_server.cpp:957-962`; `set_confirmations`, `:163-168`): a payment in the wallet's last
  block has 1. Passing the rail `height − 1` as its tip keeps `rail`'s `tip − height + 1`. **With 8-A2:** the height
  is the wallet's own, stored at every pass beside the cursor (`MoneroCursor.WalletHeight`, the `wallet_height`
  column of migration 5, in the pass's one commit, `CommitMoneroPass`); the cursor is only where the next scan starts.
  settle, the settled rule and `Rail` read it. **With 8-A6:** each count is also at most the `confirmations` wallet-rpc
  answered with the payment's mark (below). A wallet below the cursor (rescanning, or on a shorter fork) counts
  conservatively: payments wait while it catches up.
- **8-T7c: a failed check pauses the watcher (fail closed).** A pass discovers, tracks and settles only while the last
  check passed or failed with `ErrIndexBehind` or `ErrSubaddress` (the right wallet, behind on subaddresses). After
  any other failure the pass re-runs the check and ends there, with no cursor, mark or settle, and logs `ALERT: Monero
  watching is paused: the wallet failed its startup check`. A wrong wallet answers "not found" for every txid, which
  would mark every tracked payment dead and send false `order.reverted`. The cost: Monero payments wait until the
  wallet is fixed; nothing is lost, but one first seen after its order's expiry because of the pause reads `late`.
- **8-T7d and 8-T7e: a stale peer holds the final event.** `rail.PeerFactsOf` treats a peer whose view is
  stale (failing or paused) and not paid as pending, so neither rail writes a final event for a mixed order while the
  other is stale, for as long as it stays stale, past the window too. At the window's end such an order gets the
  rail's left-the-window `ALERT` and no invented event (SatBooth does not write a final event it cannot know; a
  person decides); a settle after the peer is fresh again writes it. The cost: a mixed order whose other rail was
  stale at its window's end needs a person. Applies to both rails.
- **A payment SatBooth cannot see is first seen late** (with 8-T7d): one that arrives during an outage or a pause and is
  first seen after its order's expiry is classified late, on both rails, as plan 6's rule. Payments the wallet only
  finds after a rescan deeper than the listing window (the cursor less the settled depth) are not listed at all:
  `docs/rules-monero.md` says to reconcile them from the merchant's wallet.
- **8-T8a: the Monero press mirrors the on-chain press.** `ErrUnavailable` (the not-yet-checked error included),
  `ErrIndexBehind`, `ErrSubaddress` and `ErrBusy` redirect to `?busy=1`; the "Pay with Monero" button is never hidden
  for staleness. "Checking payment…" shows for a stale rail only when it has an issued address or money seen
  (`staleChecking`, one rule for both rails, which also gates the expired page's "Nothing was charged").
- **Further rulings.** `poll_rate` and `unused_alert` 0 mean the default, as on-chain (JSON cannot tell 0 from
  absent); `poll_rate` below 1 is refused with `config: monero.poll_rate must be 1 to 10000`; the BTC reach refusal
  names the configured table providers (7-R, changed above); the both-rails `ALERT` is logged once per rail per order
  per process, so two lines when both watched rails record; the startup check is fatal (`startup failed`, reason
  `monero`, its sentinel's text as the `cause`, never wallet-rpc's), and without the `monero` block a store with an
  open Monero order refuses to start.
- **8-T10a-1: a sender's lock is not reproduced on regtest.** At v0.18.5.4 wallet-rpc refuses a non-zero
  `unlock_time` with -50 (*source*: `wallet_rpc_server.cpp:1266-1271`) and monerod's pool refuses to relay one
  (`src/cryptonote_core/tx_pool.cpp:185-191`). `TestMoneroUnlockTime` asserts wallet-rpc's -50 only (no such
  transaction can be built through wallet-rpc, so the pool's refusal is cited, not asserted) and that nothing reaches
  the order; the late-by-lock rule stays covered by `monerotest`.
- **8-T10a-2: foreign entries are skipped silently.** Entries for addresses SatBooth never issued (the primary address,
  a subaddress not in the store) are not SatBooth's orders and are skipped without the
  `wallet-rpc listed entries SatBooth could not read` line, which stays for entries that fail the shape checks or repeat
  a key in one list. A (txid, subaddress) listed both `in` and `pool` counts once, mined. The line had repeated on every
  pass while a foreign payment sat in the listing window (99 lines in one phase). A coinbase to the account (an `in`
  entry of type `block`, `fill_transfer_entry`, `wallet_rpc_server.cpp:520` at v0.18.5.4) is skipped the same way, by
  the client. **With 8-A4:** the line is an `ALERT` naming the listing's height range to reconcile by hand (below).

**Rulings 8-A1 to 8-A5 (plan 8).** Each fixes a defect that a failing test showed first.

- **8-A1: one settle lock for the watched rails.** The watched rails and lnpay share one `sync.Mutex`, installed with
  `SetPeer` and `SetSettleLock`, whenever at least one watched rail is configured (a single rail has no peer, but still
  shares the lock with lnpay). Each rail holds it for the whole of each settle. The peer's paid state changes only in
  its settle, so it cannot change between a settle's read of it and its event writes: no `order.paid_again` without
  money, and no lost `order.reverted` when both rails revert together. lnpay holds it only while it writes a payment's
  ledger transfer, which is what its status reads as paid; its `order.paid` event write and `LightningPaid` do not take
  it. Under it settle takes only leaf locks and makes no RPC call. The cost: settles of different orders on the two
  rails no longer overlap (milliseconds per order).
- **8-A2: the cursor is only where the next scan starts;** confirmations come from the wallet's height (8-T7b above).
- **8-A3: the press checks the base address after `create_address` too** (8-T7a above).
- **8-A4: the cursor still advances when entries drop** (holding it would let one bad entry grow every listing without
  bound). `ALERT: wallet-rpc listed entries SatBooth could not read; reconcile the wallet's transfers from from_height
  to to_height, and its pool, by hand` (the count, the listing's `min_height` and the tip), once per pass while it
  lasts; an entry above 2^53 − 1 piconero to an issued subaddress is not counted there but gets `ALERT: a Monero
  payment above 2^53-1 piconero to an issued subaddress is not recorded; reconcile it by hand` with its order id, once
  per payment per process (bounded per pass by R8 below). The cost: a payment in an unreadable entry is missed by SatBooth, with the money in the
  merchant's wallet and an `ALERT` saying where to look.
- **8-A5: reversals sum the payments that stand.** `rail.PlanReversals` and `rail.Stamps` sum the surviving records,
  capped once, instead of subtracting from a confirmed sum already capped at the rail's bound, and settle recomputes
  the confirmed sum with `rail.Sums` after its reversals. Only Monero can exceed its bound (2^53 − 1 piconero is about
  9,007 XMR; `MaxSat` is the whole bitcoin supply).

**Rulings R1 and R5 to R8 (plan 8).** Each fixes a defect that a failing test showed first.

- **R1: status reads take the settle lock.** `Rail` (both rails), which status, the API and the page call, holds the
  settle mutex while it reads, so it never sees a settle half done: settle records a new payment before it reverses an
  old one, and a read between them summed both (an order whose 0.6 of 1 XMR was reorged out while another 0.6 was
  mined read `paid`). `View`, the peer path that the other rail's settle calls while it holds the mutex, reads
  without it. With a single watched rail the mutex is shared with lnpay alone. A press (`Address`) still reads the peer's `View` without the
  lock: a half-done settle there can only over-count, which refuses the press (`ErrPaid`), the safe side. (Since S1
  below a settle's ledger records are one batch, so not even an interrupted settle is half done in the ledger.)
- **R5: an early plan 8 store is refused,** as above; no migration (no release exists).
- **R6: a payment's first-seen height is the wallet's height** of the pass that found it, not the cursor's, so the
  lock rule (`unlock_time > first_seen_height + 10`) is judged against the wallet while it sits below the cursor.
  Rows stored before keep their height.
- **R7: `rail.Stamps` and `rail.PlanReversals` are linear.** The surviving sums are built once, backwards: the final
  survivors' sum, then each reversed payment added back (exact under the cap, where subtracting is not). The 8-A5
  version stays in the tests as the reference they are compared with.
- **R8: the oversized `ALERT` is bounded.** At most 16 lines a pass, then one `ALERT: more Monero payments above
  2^53-1 piconero to issued subaddresses are not recorded; reconcile them by hand` with the count of the rest. Every
  payment logged or counted is remembered for the process, and the set is never cleared (when it is full, new ones
  are counted in that one line each pass). The double-spend and bad-transfer `ALERT`s
  follow the same rule (below).

**Rulings R2, RU and 8-A6 (plan 8).** Each was reproduced by a failing test, which is in the tree with its fix.

- **R2: every settle heals both rails' reversals.** A reversal is a ledger transfer and its `order.reverted` an event
  in the order store: two databases. A crash or a store error between them used to be healed only by the next settle
  of the same rail, so the other rail could settle first, see no reversal, skip the `order.paid_again` of its own
  payment and later stamp its own reversal with the same seq (two paid-to-unpaid cycles became one). `rail.Peer` now
  has `RevertedSeqs`: the seqs the peer's reversals of the order carry, read from its store and ledger only (no RPC
  call, not the settle lock). Step 1 of each settle adds the missing `order.reverted` for its own and the peer's seqs
  (`rail.HealSeqs`) before it derives anything. The cost: one more local ledger read per settle of a mixed order.
- **RU: no new record for a payment the wallet does not know this pass.** While the wallet catches up, a tracked payment
  whose transaction it does not know gets no mark, and its order was settled on its older marks: an unrecorded mined
  mark was then counted against the lower wallet height and could be newly recorded, a false `order.paid` (after a crash
  between the mark and the record, or after two moves to a shorter chain). Such a payment is not newly recorded in that
  pass; a recorded one is kept or reversed on its marks as before. The narrower of two possible rules: a payment that is
  not polled (a late one past its window, say) is still recorded from its mark when its order is settled. The cost: a
  payment the wallet stops knowing while it catches up is recorded a pass later.
- **8-A6: the staged pass.** A wallet can change while a pass reads it: wallet-rpc's
  height can go down between two calls (a refresh that meets a reorg crops the wallet's chain, `handle_reorg` and
  `detach_blockchain`, `wallet2.cpp:3446-3459`, `4475-4482` at v0.18.5.4; a daemon switch or a rescan does too), and
  a wallet replaced mid-pass answers "not found" for every txid. The pass used to write its scan and cursor before it
  tracked, and each mark right after its call, so a pass that read a changed wallet left its writes behind. Now:
  - discover and track write nothing. The new payments, the cursor `{tip, h}` and the marks are held in memory; new
    payments, and a settled dead payment listed mined (which the commit reopens), are polled in the pass that finds
    them, as before.
  - after the pass's last call, the end check reads `get_height` and the identity (`get_address` of index 0) again.
    Under the settle lock, the pass compares the wallet generation, a counter that every `walletChanged` (a press or an
    identity read that saw another wallet) moves, read before step 0. A height below `h`, another identity or a moved
    generation discards the pass: nothing is written, nothing is settled, `lastGoodPass` is not stamped, and the next
    pass reads the wallet again. Another identity also pauses the rail, as the identity read of every other pass does.
    Each discarded pass logs a WARN, `the wallet changed while the pass read it; nothing from the pass is written, the
    next pass reads the wallet again`, with `reason` `height`, `address` or `generation`; the third in a row also logs,
    once until a pass commits, `ALERT: the wallet changed during several passes in a row; nothing was written from
    them: is wallet-rpc's wallet or monerod changing?`.
  - otherwise the scan, the cursor and the marks are one store transaction (`store.CommitMoneroPass`), under the
    settle lock, and settle runs on what was committed. No RPC call is made under the lock. Since S2 the pass keeps
    the lock from the generation check through all its settles, and `walletChanged` takes it.
  - confirmations are bound to the observation. `xmrrpc` parses each transfer's `confirmations`, which wallet-rpc
    computes in the same call as the entry (`set_confirmations`, `wallet_rpc_server.cpp:163-168`); a missing key
    is 0 (`KV_SERIALIZE_OPT(confirmations, 0)`, `wallet_rpc_server_commands_defs.h:1486`, leaves it out at 0, as
    for every pool entry), and a value that is not an integer of 0 to 500,000,000 is dropped as a bad field. Each mined mark stores them
    (`monero_marks.confirmations`, migration 5 edited in place, pre-release), and settle, the settled rule and `Rail`
    count `min(observed, WalletHeight − height)` (`rail.Out.Bounded`), so a payment that track saw on a shorter chain
    which then won back within the pass is neither recorded nor settled on the stored height. "Settled" (no longer
    polled) needs the observed count at the depth too. A new mark is written when the count changes, so a payment adds
    one mark per block; the stored count is capped at the settled depth (the deepest tier), which no rule reads past,
    so the marks stop there. A later raise of the deepest tier is picked up a pass later for an unsettled payment (the
    capped count differs); a settled one is not polled again (S5 below).
  - the bound applies only until the payment is settled (since S5 read from the stored settled
    fact, not from the count against today's deepest tier): a settled payment, no longer polled, counts `W − h`, so it
    still shows a growing count in `Rail` (the API, the page, status).
    A reorg past the depth is not detected by design (Ruling G), so nothing is lost; settling still needs the observed
    count at the depth.
  - the cost: two more calls per pass, and a pass that meets a changing wallet is redone.
  - **the remaining gap:** a switch to another wallet and back (A, B, A) inside one pass with no press in between.
    Both identity reads see A and the generation does not move, so B's "not found" answers are committed as dead marks
    and settle reverses A's payments (false `order.reverted`, then `order.paid_again` once A lists them mined again).
    Identity reads cannot see it; what would close it is not trusting a single "not found" for a dead mark (requiring
    it on two passes). It needs wallet-rpc's wallet file switched twice within one pass (seconds) by someone with
    access to the private link.
- **The double-spend and bad-transfer `ALERT`s are bounded as R8.** Their sets were cleared when full, so 4,097
  distinct payments logged 4,097 lines every pass. All three per-payment `ALERT`s now share one rule: once per payment
  per process, at most 16 lines a pass, then one summary with the count (`ALERT: more Monero payments' inputs were
  seen double spent; the block decides`, `ALERT: wallet-rpc answered with more bad transfers`); the sets are never
  cleared.

**Rulings S1 to S5 (plan 8).** Each fixes a defect that a failing test showed first.

- **S1: no partial settle is durable, on both rails.** Settle wrote each forward and each reversal as its own ledger
  transaction, so a settle that ended between a replacement's forward and the replaced payment's reversal (an error,
  a cancellation, a crash) left both standing: `Rail` read the order paid (0.6 + 0.6 of 1 XMR), and the next settle's
  paid-before check published a false `order.paid`, then `order.reverted`. The ledger now has `CreateTransfers`: the
  transfers in order in one SQLite transaction, an identical existing one a no-op, and a refusal of any item rolls the
  batch back (TigerBeetle's linked chain: the refused item gets its code, the others `linked_event_failed`; a
  transient refusal is not recorded in `failed_transfers`, but an expiry the batch observed is kept, as
  `CreateTransfer` keeps it). Each settle plans its forwards and reversals in memory, the views updated as before, and
  writes them as one batch, then the events. A forward that exists with other details is logged and left out, and
  the rest is planned again and retried; a reversal that does fails the settle (TU1 below); any other refusal is an
  error. The test hooks between transfers are gone; one, `settle:after-transfers`, sits between the batch and the
  events.
- **S2: invalidation is serialised with the commit and the settles.** `walletChanged` moved the generation without
  the settle lock, so a press could land between the generation check and the commit, or between the commit and the
  settles. It now makes its bump under the lock (no RPC call under it; none of its callers holds it), and the pass
  takes the lock once: the generation check, the commit, every settle of the pass, and the clearing of the pending
  set (S4), then releases it. A press that sees another wallet meanwhile waits until the pass's settles end; `Rail`
  waits for the whole section too, not for one settle. A press that saw B while the pass's end identity read saw A
  (B came and went between the press and that read) moves the generation before the check, and the pass is
  discarded; one that saw B but whose `walletChanged` had not run yet when the pass's lock was taken is applied after
  the settles, and the pass committed what it read from A: that window belongs to the A, B, A gap above.
- **S3: FirstSeen is the clock at discovery.** The staged pass stored a new payment with the store's clock at the
  commit, after every paced tracking call, so a payment discovered a second before the expiry could be stored first
  seen after it and classified late for good. Discovery now stamps each kept entry with the clock read when
  `get_transfers` answers, and `CommitMoneroPass` stores the caller's first-seen time for a new row (a missing one is
  refused); a stored row keeps its own.
- **S4: the orders a commit touched stay pending until they settle.** A crash between the commit and settle left a
  late payment found already deep unsettled for good once its window closed: it was not polled (late, past the
  window), discovery skipped its stored row, and nothing brought its order back. Migration 5 (edited in place,
  pre-release) gains `monero_pending_settle`, a work queue of the store (its rows
  are deleted, never updated; `webhook_abandoned` is the other). The
  commit records the orders it touched, less those skipped for a failed call (which the pass does not settle either:
  an order whose payment keeps failing is reported when its window closes, never given an invented final event) but
  with the skipped ones for which it writes a new or changed mark (T1 below), in its transaction; each pass settles the pending orders whatever their window, and clears an order once it settled.
  A pending row keeps the Monero rail required at startup (`HasOpenMoneroOrder`). A version-5 store without the table
  is refused (`ErrOldMoneroSchema`). The cost: one insert and one delete per touched order per pass.
- **S5: a settled payment's count is unbounded by its settled fact.** `Bounded` came from the stored count against
  today's deepest tier, so raising that tier after a payment settled froze its displayed count. It now comes from the
  stored settled fact. A settled payment stays settled when the tier it would now need is deeper (raising the tier
  does not reopen it).
- **The tier-1 test of the settled rule.** `TestHeightDropsAndRecoversWithinPassTierOne` fails if
  the settled rule stops requiring the observed count at the depth.

**Rulings T1, T3 and TU1 (plan 8).** Each fixes a defect that a failing test showed first.

- **T1: a skipped order with a committed mark stays pending.** S4 left out every order skipped for a failed call,
  even when the commit wrote a mark for another of its payments. A late payment observed mined in the window's last
  pass, beside a sibling whose call failed, was then never settled: past the window neither payment is polled,
  discovery skips stored rows, and the order's final event keeps it out of the expiry fallback. The pass now records
  such an order pending too; it is still not settled in that pass, and the next pass that does not skip it settles
  it whatever its window. An order whose payments all failed stays out, as before.
- **T3: the reversals a settle writes read reversed in its views, on both rails.** A planned reversal cleared
  `Recorded` and left `Reversed` false, so step 5's `rail.LateRecorded` missed a late forward reversed in the same
  call. A recovery that reversed a late forward whose `order.late` a crash left unwritten (the payment reorged out
  meanwhile) then cleared the pending row without the event, and past the window nothing settled the order again.
  Once the batch is written, the views of its reversals set `Reversed`; `Code`, `Timestamp` and `Seqs` still describe
  the record as loaded.
- **TU1: a refused reversal fails the settle, on both rails.** On the collision retry a reversal refused as
  `exists_with_different_*` was left out while the plan around it was kept, so the replacement's forward could be
  written beside the record it replaces, and a false `order.paid` published (it needs a writer between the settle's
  ledger walk and its batch; a test inserts one). Such a refusal now logs `ALERT: a payment record exists with
  different details` and returns an error: nothing is written, and the order stays pending (Monero) and stale, so a
  person puts the record right. A refused forward keeps the leave-out-and-retry.
- **A payment that leaves its window unobserved is alerted.** A late payment whose
  `get_transfer_by_txid` fails in every pass of its window has no mark; once the window closes it is not polled and
  nothing records it, and `logNoFinal` is silent when its order already has its final event. The pass now logs
  `ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet`
  with the order id (`store.MoneroPaymentsLeftUnobserved`, over the same window ends as the no-final-event line),
  whatever the order's events: once per payment per process, at most 16 lines a pass and one summary for the rest
  (`ALERT: more Monero payments left their watch window without one successful observation; reconcile them from the
  wallet`). An on-time payment is not listed: it stays polled past the window. A payment observed only in passes
  that were discarded, or only by a wallet catching up (`errNotKnown`), has no mark either and is alerted too, the
  conservative side.
- **The batch property test's cost.** `TestCreateTransfersProperty` compares balances with the model only after a
  refused batch that created an item before its refusal, and runs 1,200 iterations: 24 s under `-race` instead of
  116 s.

#### After the watch window: by design

When an order's watch window closes, SatBooth stops watching it. A payment that has not settled by then (one that
never confirms, or a late one the wallet could not be asked about in time) keeps its stored record, which the API
and the page show. SatBooth does not hold the order open for it: the merchant settles it or returns the money by
hand, from their own wallet.

### Lightning refunds, as built (plan 9)

SatBooth pays a Lightning refund of an order paid over Lightning, in msat, to an invoice the claimant gives, when a
person has approved it. The code is `approval` (the approval formats, the configured keys and their checks),
`lninvoice` (the claimant invoice's syntax check and its amount), `refund` (admission, the sender, the resolver, the
void and start-up recovery), `store` (the refund tables), `api` (the refund routes and the order's Lightning figures),
`service` (the `payout` block, the second NWC connection and the start-up checks) and `cmd/satbooth-approve` (the
signing tool). `docs/configuration.md` and `docs/refunds.md` have the operator's and the merchant's view ("The `payout`
block", "Refunds", "`satbooth-approve`"). The threat model, [`threat-model.md`](threat-model.md) §4, is the
specification: the approval layouts, the limits, the claimant invoice's checks, the state machine and the API surface
are as it states, and this section adds only how the code meets it.

1. **Two connections.** The invoice connection makes and looks up invoices; the payout connection pays. The payout
   connection is a second NWC connection (its own wallet key and client key) with the scopes `pay_invoice`,
   `lookup_invoice` and `get_info` and a lifetime budget the hub enforces. With the `payout` block SatBooth dials it
   beside the invoice connection, through a relay pair of its own.
2. **Start-up checks.** `satbooth` refuses to start when: the payout connection is the invoice connection (by wallet
   key or client key); the payout connection's `get_info` methods lack `pay_invoice` or `lookup_invoice`, or hold a
   method outside those two, the methods the hub grants with `pay_invoice` (`pay_keysend`, `multi_pay_invoice`,
   `multi_pay_keysend`) and those it grants to every connection (`get_info`, `get_budget`); the payout connection has
   no budget or one that renews; the invoice connection's `get_info` lists no methods, lacks `make_invoice` or
   `lookup_invoice`, or lists a method outside those the hub reports for SatBooth's invoice pairing (any pay method
   among them; the error names it); or the store holds an open refund and the configuration has no `payout` block.
   The invoice connection's check runs on every start, with or without the `payout` block, so the hub must answer
   `get_info` within 30 s for SatBooth to start at all. Both connections' `get_info` must also state the network
   `nwc.network` names, and that comparison is made after the methods checks ("The final fixes, as built (plan 14)"
   below); the network of each invoice is checked in addition (`nwc.network`; the threat model, §3).
3. **Admission makes no hub call.** `POST /v1/refunds` verifies the approval, then, under the one transition mutex,
   checks the refund id, the order, what is left to refund on it, the invoice's syntax, that it is not one of
   SatBooth's own invoices (`own_invoice`, whatever its amount), that it carries exactly the approved amount (else
   `invalid_invoice`, an amountless invoice included: getAlby/hub `921003b`
   `transactions/transactions_service.go:180-181` refuses to make an invoice under 1 sat, so paying an amountless one
   cannot be qualified), the daily cap and the open-refund cap, and stores the refund `claimed` with the approval
   bytes and the invoice string. It answers 202 at once. The hub checks run in the sender, so a slow hub never holds
   an API request: the server's write timeout is 15 s, and a hub call is bounded by 15 s in the workers.
4. **The sender.** Woken by a claim, and every 5 s. Each round first re-checks, for every claimed refund oldest first,
   the approval's expiry and key against the configuration in force, from the store alone (an expired approval ends the
   refund `expired_unsent` on any round, whatever the hub does). Then it reads the budget once: a refund without
   headroom (the amount plus the larger of 1 % and 10,000 msat) gets an `ALERT` and waits, and is skipped without
   holding back the others. Of the refunds the budget covers it takes the oldest that is not in an in-memory skip set: a
   refund enters it when its lookup fails (an answer with another invoice, or none, counts as failed and gets an
   `ALERT`), or when the steps after a good lookup stop with an `ALERT` (the ledger already resolved its pending,
   refused the transfer, or the store refused the marker); it leaves it on its next good lookup, and when every refund
   the budget covers is in it, their entries are cleared and the sender starts again from the oldest. It looks the
   invoice up on the payout connection (a hub that answers with a row of the refund's own invoice, of any type or
   state, ends the refund `invoice_known`), creates the ledger's pending transfer, writes the send marker, and sends
   `pay_invoice` once, with a request that
   expires at the marker's `sent_at` plus `payout.timeout_seconds` (whenever it is built, so the 11-minute void wait
   always outlasts it); if that time has passed when the sender reaches the call, or the client finds it passed, nothing
   is sent and the refund ends `not_sent`. So a round makes at most 2 hub calls before its one send (the budget read and
   one lookup), however many refunds are claimed, and 1 when the budget read fails or the budget covers none; one refund
   is sent a round, and neither a hub error nor such an `ALERT` on one refund blocks the rest. A failed store, ledger or
   hub call is logged once per refund and condition, not every round. A relay refusal of that request ends the refund
   `not_sent`; every other answer, a success included, is left to the resolver.
5. **The resolver.** Every 30 s it looks up each sent refund, at most one lookup a second, and acts only on an answer
   that is outgoing, of the refund's invoice and of its amount: `settled` posts the pending transfer and settles the
   refund; `failed` voids it and fails the refund; `pending` waits (one `ALERT` after an hour); no record and
   `expired` wait for a person (one `ALERT`); anything else is an anomaly (one `ALERT`, nothing changes).
6. **The void.** `POST /v1/refunds/{id}/void` takes a signed void approval and ends any open refund the hub does not
   report pending; its rows are in "The void, as built (plan 11)" below. On a `sent` refund it acts only once the send
   marker is at least 11 minutes old (`nwc.MaxTimeout`, the cap on `payout.timeout_seconds` and so on a refund request's
   lifetime, plus a minute: by then a relay whose clock agrees with SatBooth's serves the request to no one), and
   decides with a fresh lookup (bounded by 8 s): no record or `expired` fails the refund `voided`; `settled` and
   `failed` resolve it as the resolver would; `pending` answers `refund_in_flight`. While SatBooth's sender can still
   publish the refund's request (from the send marker until its `pay_invoice` call has returned, or it decided not to
   make it), a void also answers `refund_in_flight`, whatever the clock reads: the sender marks the refund in flight in
   the critical section that writes the marker, and the void checks the mark under the same mutex before its lookup and
   again before it writes. The mark is kept in memory, which is enough: the data directory's lock makes the process the
   only sender, a sender that crashed publishes nothing, and after a restart a refund with a marker is never sent. It
   covers a sender paused between the marker and the publish, not a request already published (threat model §4).
7. **Ledger and store.** A refund's pending transfer moves msat from the merchant's Lightning account to the hub
   account; it is created just before the send marker, posted when the refund settles and voided when it fails (a void
   of a claimed refund voids it too, if it exists). The store enforces the allowed set a second time: the approval's
   fields against the row, one refund per invoice, and the order of the times (sent after claimed and before the
   approval's expiry, a void at least 11 minutes after the send).
8. **Start-up recovery.** Before the workers start, every open `sent` refund whose pending transfer the ledger shows
   posted is settled, and every open refund whose pending it shows voided is failed (`recovered`). A posted pending
   on a `claimed` refund raises an `ALERT` ("a person voids it") and changes nothing, and so does a pending the
   ledger shows both posted and voided, which the ledger never produces. No hub call is made, and nothing is ever
   sent again.
9. **What the merchant sees.** The refund view (`GET /v1/refunds/{id}`, polled; there are no refund webhooks) and the
   order's `lightning.paid_msat`, `lightning.refunded_msat` and `lightning.refund_open_msat`, which the order view
   carries whether or not the `payout` block is configured, so a refund already paid stays visible after it is
   removed.
10. **Logs.** Fixed messages with the refund id and the order id only, never an invoice, payment hash, preimage,
    approval or key. Every condition a person must act on is an `ALERT:` line, a constant in `refund`.
11. **What is deliberately not handled.** A refund that settles after a person voided it keeps its `failed` record,
    as a payment past its watch window keeps its own; the merchant reconciles it by hand. Routing fees come out of
    the payout connection's budget, not the merchant's account in the ledger. Refunds of on-chain and Monero payments
    are paid by a person and not recorded by SatBooth.
12. **Operators.** A hub shut down while a refund's payment is in flight can mark it `failed` and settle it after its
    restart. Do not restart the hub while a refund is in flight; if it was restarted, check the hub's own record of
    the payment before approving a failed refund again. The new approval a person must sign is the guard.

The regtest qualification (`qualify/REPORT.md`, "Lightning refunds") pays refunds through two real hubs, refuses
the partial that exceeds the order, SatBooth's own invoice and an invoice the hub already paid, fails a payout the hub
reports `failed` and pays it again under a new approval, kills SatBooth between the send marker and the request and
voids that refund and one whose request was cut, and holds a refund larger than the budget left.

#### The void, as built (plan 11)

A person can end any open refund with a signed void; the one refund a void cannot end is one the hub itself reports
pending. The void approval, the route and `satbooth-approve void` are the ones above. The code is `refund/pay.go`
(`Void`, `voidChecks`, `voidRow`, `claimedResult`, `sentStep`) and `store` (two result reasons); the threat model's
§4 state machine lists the same rows as transitions.

**Checks, in this order**, each with its answer:

1. the approval verifies and names the refund: 400 `invalid_approval` or `approval_expired`;
2. the refund exists: 404 `not_found`;
3. it has no result, and no decided result is waiting for its write: 409 `invalid_state`;
4. SatBooth's sender cannot still publish it (the in-flight mark): 409 `refund_in_flight`;
5. the ledger's reading of the refund's pending transfer: **N** none, **P** pending (neither posted nor voided),
   **S** posted, **V** voided (or both posted and voided). With the store's state, claimed or sent, it picks one row
   of the table below. Every other pair, V on either state and S on a sent refund, answers 409 `invalid_state` with
   `AlertVoidLedgerResolved` and writes nothing: start-up recovery ends a voided pending (`recovered`) and a posted one on
   a sent refund (settled); a pending both posted and voided only raises an `ALERT` there, and the ledger never
   produces it.

Checks 2 to 5 run under the transition mutex; a store or ledger read that fails answers 500 `internal`.

| Row | Store | Ledger | Hub | Written |
|---|---|---|---|---|
| V1a | claimed | N | none | the refund's pending is created and voided, then failed `cancelled` |
| V1b | claimed | P | none | the pending is voided, then failed `cancelled` |
| V1c | claimed | S | none | settled (reason `ledger_paid`); no ledger write |
| V1d | sent | P | from `sent_at` + 11 min, one fresh lookup | settled: the pending is posted, settled; failed: the pending is voided, failed `failed`; no record or `expired`: the pending is voided, failed `voided` |
| V1e | sent | N | from `sent_at` + 11 min, one fresh lookup | settled: the refund's pending transfer is created, then posted, settled; failed: failed `failed`; no record or `expired`: failed `voided`; no ledger write for either failure |

- **V1a to V1c make no hub call.** The checks, the choice of row and its writes run in one hold of the mutex, on a
  context detached from the request and bounded by 5 s (`VoidWriteTimeout`). The sender writes the send marker and
  the in-flight mark under the same mutex before its one `pay_invoice`, so a refund without a marker was never sent
  from this data directory, and a sender that reads after the void finds the result and sends nothing.
- **V1a moves the ledger first.** It writes the refund's pending (the transfer `prepare` and V1e use) and its void
  (the transfer V1b writes) in one ledger batch, applied whole or not at all, then the result, with the crash point
  `void` after the batch. A pending the ledger refuses (neither created nor exists, as the sender's `prepare` meets it
  too; the void's item then answers `linked_event_failed`) leaves nothing to void: the cancel is the result row alone,
  with no `ALERT`. A refused void item (impossible for a fresh pending) raises `AlertLedgerRefused` and the void answers
  500 `internal`. A ledger call that errors answers 500 `internal`, writes nothing (not even the pending), leaves
  nothing waiting and logs no retry line; a second void runs the row again.
- **V1d and V1e.** After the checks, a send marker younger than 11 minutes (`VoidAfter`) answers `refund_in_flight`.
  Then the mutex is released for one lookup, bounded by 8 s (`VoidLookupTimeout`), read as the resolver reads one:
  only an outgoing row of the refund's invoice and amount counts. `pending` answers `refund_in_flight`; an answer
  that does not match the refund (another invoice, amount or type, an unknown state, a malformed answer) answers 503
  `hub_unavailable` with `AlertAnomaly`; an error answers `hub_unavailable`. The mutex is then taken again and checks
  2 to 5 repeat, the in-flight mark included; a refund whose state or ledger letter differs from the first reading
  answers `invalid_state`.
- **V1e, settled,** is the only case in which a void creates a ledger transfer. The pending transfer is built by the
  helper the sender's `prepare` uses (`createPending`: same id, accounts, amount, order in `UserData128`, ledger, code
  and flag), then posted. A ledger that refuses the pending raises `AlertLedgerRefused` and nothing is written. A post
  refused after the pending was created raises `AlertLedgerRefused` (`AlertLedgerOpposite` if the ledger reads it
  voided); the pending stays, nothing is rolled back, and the refund is then in V1d's state: the resolver's next
  lookup or a later void ends it. Either way the void answers 500 `internal` and the refund stays `sent`. A crash
  after the pending also leaves V1d's state; a crash after the post is settled by start-up recovery.
- **A result the store refuses waits with its row.** The void answers 500 `internal`, and the decided result and its
  ledger step wait in memory (`AlertResultRefused` for a refusal, a log line for any other failure). Every sender and
  resolver round first writes it again: the same result with the same ledger step (the create and void for V1a; none
  for V1c, V1e's two failures and V1a's fallback; the void or post of the pending for V1b and V1d; the create and post
  for V1e settled; each step is idempotent), never another row's. A second void meanwhile answers `invalid_state`. The
  wait is not kept over a restart: start-up recovery reads the ledger again, so a voided pending (V1b, V1d's failures)
  ends `recovered`, a posted one on a sent refund (V1d and V1e settled) is settled, and a refund whose row wrote no
  ledger transfer (V1e's failures, V1a's fallback) is open again as it was before the void; a V1c refund raises
  `AlertPostedUnsent` again. A cancel (V1a, V1b) whose result the store refused therefore survives a restart as
  `recovered`, never sent. The one limit is V1a's fallback, where the ledger refused the pending: its refused result
  is held in memory alone, and after a restart the refund is claimed again (the sender's `prepare` meets the same
  refusal and does not send it).
- **A crash in V1a or V1b** between the ledger's void and the result leaves a voided pending on a claimed refund: the
  next start ends it `recovered`, not `cancelled`. V1a has no crash point between its pending and its void: they are
  one batch.
- **Two reasons** (migration 6): `refund_results` accepts (`failed`, `cancelled`) and (`settled`, `ledger_paid`), each
  only on a refund without a send marker (the `refund_results_needs_no_send` trigger and `refundNeedsNoSend`). The API
  shows `cancelled` as the reason of a failed refund; a settled refund shows no reason, so `ledger_paid` is in the
  store only. A cancelled refund is failed, so its amount is free again; a `ledger_paid` refund is settled and counts
  as refunded. `store.Open` refuses a database whose `refund_results` predates the two reasons
  (`store.ErrOldRefundSchema`).
- **Nothing repairs itself.** The sender, the resolver and start-up recovery write no new transition; only a signed void
  does. `AlertNoPending` (a sent refund without a pending: V1e) and `AlertPostedUnsent` (a posted pending without a
  send marker: V1c) end "a person voids it", and `AlertLedgerResolved` "restart SatBooth to finish it or void it if
  the ledger shows it paid" (a void the ledger's state refuses raises `AlertVoidLedgerResolved`, "restart SatBooth to
  finish it"; the sender raises `AlertLedgerResolved` for a claimed refund whose pending is posted, which a void ends
  as V1c, or voided, which a restart ends `recovered`). Each is logged once per refund while the process runs, and
  again after a restart.

The regtest qualification (`qualify/REPORT.md`, "Lightning refunds", `TestRefundBudget`) voids a claimed refund held
by the budget: 200, failed `cancelled`, its order's amount free again, no ledger transfer and no send marker, the
payout budget unchanged, and a second void 409 `invalid_state`.

### Operations, as built (plan 10)

**Built (plan 10).** The code is `alert` (the `slog.Handler` that queues
`ALERT:` lines), `store` (the pair stamp, migration 7; the alert tables, migration 8), `ledger` (the pair stamp,
migration 4), `webhook` (the deliverer, generalised over events and alerts), `service` (the stamp check, `satbooth
backup`, the `alerts` block and the wiring), `cmd/satbooth` (the `backup` subcommand), `checkout` (the logo) and
`deploy/` (the release images and one Compose bundle per host). The operator's view is in `docs/backup-and-restore.md`,
`docs/alerts.md` and `deploy/README.md`; the qualification is in `qualify/REPORT.md` ("The release
images (plan 10)", "Deployment bundles"). The threat model's §0, §5, §6 and §9 carry the security side.

1. **Release images.** SatBooth ships as no-distro containers built `FROM scratch` by `deploy/image/Dockerfile`
   (targets `satbooth`, `satbooth-relay` and `satbooth-tools`): a static binary and only what it needs, run as the numeric user
   65532:65532 with no shell, no package manager and no passwd file. The builder is the digest-pinned
   `golang:1.27.4-trixie`, with `GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0` and `go build
   -trimpath -ldflags=-buildid=`. The `satbooth` image holds `/satbooth` and `/etc/ssl/certs/ca-certificates.crt`,
   copied from the builder (rate providers, webhooks, `wss://` relays and an `https://` bitcoind or wallet-rpc); the
   `satbooth-relay` image holds `/satbooth-relay`; the `satbooth-tools` image holds `/satbooth-setup` and `/satbooth-pair`
   (no entrypoint, no CA bundle: neither uses the network). `scripts/image-check.sh` lists every entry in every layer of the
   exported image and refuses any other file; `scripts/image-run-test.sh` runs each image with a read-only root, all
   capabilities dropped and `no-new-privileges`, and checks its refusal of a missing configuration (for the tools image,
   `/satbooth-setup` without arguments), its user and its environment. CI runs all three (`.github/workflows/image.yml`); nothing is pushed to a registry. `deploy/README.md`
   states the reason of each file and each pin. Both databases keep SQLite's temporary data in memory (`temp_store`),
   because the release image has no writable temp directory.
2. **The pair stamp.** The store and the ledger each hold a `pair` row (id, generation and nonce; the ledger also a
   `pending` generation and its nonce), and SatBooth refuses to start on two databases that are not from the same
   moment. The nonce is 16 random bytes in lowercase hex, drawn by each advance and written with the new generation
   into both databases; it is NULL exactly while the generation is 0, and the ledger's pending generation and pending
   nonce are both NULL or both set (CHECKs). A restore takes both files back to an earlier generation, and the next
   start reaches generations that backups of the timeline given up already have; their nonces differ, so a file of one
   timeline next to a file of the other is not a pair. The first
   stamp, and the finish of an interrupted one, is always followed by the advance in the same start, so no completed
   start leaves a generation at 0; a lost `store.db` or `ledger.db` comes back as a fresh file with no id and
   generation 0 and must land in "anything else", never in "finish the first stamp". The check runs after the
   migrations and before any worker, recovery or listener, and allows exactly these states (L the ledger, S the store,
   N a nonce, P and PN the ledger's `pending` and `pending_nonce`):

   | State | Store | Ledger | Action |
   |---|---|---|---|
   | first stamp | id NULL, generation 0, N NULL | id NULL, generation 0, N NULL, P NULL | new id: ledger, store, then advance |
   | finish the first stamp | id NULL, generation 0, N NULL | id set, generation 0, N NULL, P NULL | the store takes the id, then advance |
   | equal | id, generation and N equal to L's (N NULL only at generation 0) | P NULL | advance |
   | ledger marked | id, generation and N equal to L's | P = generation + 1, PN set | the store takes (P, PN); the ledger commits, then advance |
   | store written | id = L's, generation = L.P, N = L.PN | P = generation + 1 | the ledger commits (generation, N := P, PN; P cleared), then advance |
   | anything else | | | refuse to start |

   An empty nonce is never a wildcard: above generation 0 a stamp without a nonce is refused, and at generation 0 both
   must be empty. The advance from generation G draws a fresh nonce N: L.pending = (G+1, N), then S = (G+1, N), then
   L = (G+1, N) with pending cleared, each its own committed transaction writing a database's generation and nonce in
   one statement. A random source that fails refuses the start before any write. A refusal says which file is older
   (by generation), or that the two are not a pair when the ids differ or the generations are equal with other nonces
   (so a store at L.P with another nonce than L.PN is "older ledger"). There is no override: the operator restores one
   backup's matched pair (`docs/backup-and-restore.md`, "The pair stamp"). The stamp does not catch a pair restored
   together from before plan 10, or a pre-plan-10 file next to a recreated empty one, before the first plan-10 start;
   nor disagreement in content, such as a hot copy of a single file restored at the same generation, which is why a
   backup is offline.
3. **Backup.** `satbooth backup -config <path> -to <dir>` writes a matched pair and a manifest, offline, and never
   migrates (`docs/backup-and-restore.md`, "Backup and restore"). It takes only equal ids, equal generations of at least
   1, equal nonces and no pending value; a mismatch gets the start's refusal, an incomplete state "start SatBooth once
   to finish its last start". Restore is a documented procedure, not a command.
4. **Alerts.** A `slog.Handler` wrapper offers every `ALERT:` record, unchanged in the log, to a bounded in-memory
   channel of 256 entries that one goroutine drains into the store; the optional `alerts` block delivers them as signed
   webhooks (`docs/alerts.md`, "Alerts"). A full channel counts the alert and logs `alert queue overflow` with the
   count. The daily bound of 1,000 alerts is counted from the store, so a restart does not reset it. The handler never
   logs an `ALERT:` line itself and never writes to the store inside a log call; everything the alert queue and
   deliverer log, a failed alert write included, carries no `ALERT:` prefix. The fields are an allowlist, exactly the
   keys the `ALERT` lines log, each with a value shape: 32-hex ids (`order`, `order_id`, `refund_id`, `event`), integers
   from 0 to 2^53 (`count`, `step`, `from_height`, `to_height`, `unused`, `passes`), a signed 32-bit `code`, one word of
   `[a-z][a-z0-9_]*` up to 40 characters (`reason`, `method`, `result`, `low`, `high`) and 3 uppercase letters (`asset`,
   `currency`); anything else is dropped and counted. Repeats of an alert in its UTC hour are hits on it, at most 100 in
   all and none once it is delivered, so no table grows without bound.

   The handler wraps the service's logger right after the store opens and the pair check passes; lines logged before
   that are only logged. The drain starts there too, so the alerts of a start that is refused later (the hub down, for
   example) are stored and delivered by a later start that runs. The deliverer is a second instance of the merchant
   webhooks' deliverer over the alert tables, with the same retry policy (10 s doubling to 1 h, inside 24 h), the same
   `SatBooth-Signature` header, and its own secret: the configuration refuses an `alerts.url` that names the same
   endpoint as `webhook.url`, and an alert secret equal to the webhook secret (since plan 13 the comparison is with
   every `webhooks` entry). The alert payload is
   `{"id","type":"alert","message","fields","dropped","repeats","created_at"}`, the fields JSON with sorted keys; an
   alert's id is the SHA-256 of its message, its fields and its UTC hour, cut to 32 hex digits.
5. **Per-host bundles** (`deploy/hub/compose.yml`, `deploy/public/compose.yml`; `deploy/README.md` has the setup).
   Every value the operator gives is an address or a path in an env file copied from `example.env`; configuration and
   secrets are host files bind-mounted read-only, never an `environment:` value or argv (Compose ignores a `secrets:`
   entry's owner and mode outside Swarm). A one-shot `volume-init` per bundle (root with only `CHOWN`, `FOWNER` and
   `DAC_READ_SEARCH`, no network) gives each volume and file to the user that reads it and each directory mode 0700,
   and never changes a file's mode, so a loose secret file is still refused by whatever reads it. Every long-running
   service runs with a read-only root, all capabilities dropped and `no-new-privileges`.
   - **The hub host** runs Alby Hub v1.24.6 (the qualified image, pinned by digest), `satbooth-relay` from the release
     image, bitcoind (Bitcoin Core 32.1), Fulcrum 2.1.4 as the hub's Electrum server and, with the `monero` profile,
     monerod and SatBooth's view-only monero-wallet-rpc (v0.18.5.4). bitcoind, Fulcrum and the Monero binaries are the
     official release tarballs, checked against pinned SHA-256 hashes, on a digest-pinned `debian:13-slim`, each under
     its own user (bitcoind 10001, monerod and wallet-rpc 10002, the hub 10003, Fulcrum 10004, the relay 65532). A
     `netns` container holds the network namespace that the hub and the relay share, so the relay's hub listener stays
     on loopback and either can restart alone. The hub's address lookups (its address API and swaps) go to the
     bundle's Fulcrum (`LDK_ELECTRUM_SERVER`), not to Alby's server; its Lightning node follows the chain from bitcoind
     alone. monerod runs with `--restricted-rpc`, which `compose.yml` adds where the operator's file cannot drop it;
     wallet-rpc runs with full RPC behind its Digest login (SatBooth refuses a restricted wallet-rpc), the login read
     from a 0600 file into its environment by its entrypoint, never argv.
   - **Three networks on the hub host.** `hub` holds the hub's namespace and bitcoind; `electrum`, internal (no route
     out), holds the hub's namespace, bitcoind and Fulcrum; `monero` holds only monerod and wallet-rpc, so no other
     container can call monerod's RPC.
   - **The public host** runs `satbooth` from the release image with `ulimits: memlock: -1` and a 60 s stop grace, on
     a fixed address of the bundle's own network (SatBooth refuses a wildcard listener), and `satbooth-oneoff` (profile
     `oneoff`, no network) for `satbooth backup`.
   - **Published ports.** Every published port is written `"${ADDR:?…}:<port>:<port>"`, so an empty address is an
     error and never every interface; `scripts/check.sh` checks both bundles with `docker compose config`, refuses
     any other variable form and a wildcard host, and checks that an empty address is refused. On the hub host's
     private address (`HUB_PRIVATE_ADDR`, the private link to the public host): the relay's client listener (7448),
     bitcoind's RPC (8332) and, with the `monero` profile, wallet-rpc (18083), and nothing else. On the public host:
     the checkout page on `PUBLIC_ADDR:PUBLIC_PORT`, and nothing else; the merchant API is reached on the bundle's
     network.
   - **The hub's admin UI** (8080) is published on its own address, `HUB_ADMIN_ADDR`, loopback in `example.env`, and
     reached through an SSH tunnel; never on the private link, because the public host reaches that link and the UI
     controls the node's funds. A fresh hub belongs to whoever opens its setup page first, so the first `up` starts
     only the hub and what it needs, and the operator sets its unlock password before the rest starts.
   - **Restarts.** Every long-running service has `restart: unless-stopped`: Docker restarts it when it exits and after
     the daemon or the host restarts, but not after a manual stop. `on-failure` would leave SatBooth down after a
     reboot, since it exits 0 when the daemon stops it. SatBooth refuses to start while the hub does not answer, so
     Docker restarts it in a loop until the hub answers; Compose sets no restart delay outside Swarm, so the loop is as
     slow as each refused start (measured in `deploy/README.md`, "Restarts"). None of these refusals is an `ALERT:`
     line: the operator watches the restart count or the `startup failed` lines. Outside Docker, the systemd advice of
     "Operation" above stands.
6. **Qualification on the images** (a hybrid; `qualify/REPORT.md`, "The release images (plan 10)"). The relays always
   run from the release image. A phase group drives a `satbooth` container of the release image over HTTP only: the
   start-up checks against the real hubs, a payment on each of the three rails, a refund, a second container refused by
   the data-directory lock, a restart, and backup and restore (a matched pair restored twice; a mixed pair refused with
   exit 1 and the older-file message). The phases that need in-process hooks (an injected clock, the fake rate server,
   the wrapped payout wallet, the wallet-rpc kill, store reads after a stop) keep running `service.Run` in the qualify
   container, built with the image's flags (`TestBuildFlags` checks them). No fiat phase runs on the image: the rate
   providers need `https`, which the closed regtest network cannot reach. `TestDeployPublic` brings up
   `deploy/public/compose.yml` unmodified against the regtest hub side and checks the running container's options, its
   one published port, a payment, a backup and its restore. `scripts/qualify.sh --only deploy` runs the setup and that
   group alone, `--only image` the setup, the on-chain and Monero setup phases and the image's phases, `--only refund`
   the setup and the refund phases, `--only hub` the setup and `TestQualify` with the check on the hub's unknown-app
   lines, and `--only onchain` the setup and every on-chain phase, for a targeted run; none is the qualification. The
   hub bundle is checked by `docker compose config` and was brought up once by hand on regtest.
7. **The logo.** A flat mark and wordmark in `docs/logo/` (SVG, light and dark), with PNGs rendered from the committed
   SVGs by `docs/logo/render.sh` (resvg 0.49.0, a local tool; `--check` requires the committed PNGs to be
   byte-identical). The mark's three squares are the three rails: Lightning in front (yellow, with the bolt), on-chain
   bitcoin behind it (bitcoin's orange) and Monero at the back (the grey of Monero's own logo). The wordmark's letters
   are Rubik ExtraBold outlines converted to paths (SIL OFL 1.1; `ACKNOWLEDGEMENTS.md`), so no font is shipped. The
   checkout page embeds the mark (`checkout/logo.svg`, 515 bytes, no script, external reference or `style` attribute),
   serves it at `/static/logo.svg` as its header image and icon, and writes "SatBooth" beside it as HTML text.

### The setup command, as built (plan 12)

**Built (plan 12).** `satbooth-setup` writes every configuration file of a host and generates every
secret, so an operator answers a few questions instead of writing the files of `deploy/README.md` by hand (which stays
possible). The code is the package `setup` (the pure parts: the secrets, the `rpcauth` line, the pairing links, the two
handoff formats, the answers and their rules, the renderers of every file, the stage machine) and `cmd/satbooth-setup`
(flags, prompts, exit codes and the printed next steps), run by `deploy/setup.sh` in the `satbooth-tools` image. The
operator's view is `deploy/README.md` ("Set up with `satbooth-setup`") and the guide `docs/migrating-from-btcpay.md`;
the threat model's §9 ("The setup command (plan 12)") carries the security rules; the qualification is in
`qualify/REPORT.md` ("Deployment bundles").

1. **What it supports.** The two bundles as they are: a new hub with its own bitcoind, on the hub host, and SatBooth on
   the public host. The bundle's dedicated bitcoind is the only supported node. An existing Alby Hub, a hub on LND or
   Core Lightning, a hub hosted by Alby and an existing bitcoind are not supported. A migration guide takes a BTCPay
   Server merchant across (a guide only: no tooling and no API compatibility layer). The networks it accepts are
   `mainnet`, `signet` and `regtest`; `testnet` is refused with "set up testnet by hand (deploy/README.md)".
2. **Once per host, and a separate binary.** Each secret is generated on the host that reads it, from one source of
   randomness (`crypto/rand`): the NWC client keys, drawn again in the (negligible) case of an invalid scalar, come from
   the same reader as every other secret. Only public values cross hosts: the `rpcauth` line of SatBooth's bitcoind
   user, the NWC client public keys, the hub's Wallet Public Keys, and the public host's copies of the network and the
   addresses (compared, never used). The one exception is monero-wallet-rpc's Digest password, which both hosts need in
   plain text: it crosses as a 0600 file the operator copies. `satbooth-setup` is a separate binary in a tools image
   with `satbooth-pair`; the service binaries do not change. `satbooth-approve` is not in that image: the admin's
   signing key lives on neither host, and setup takes approval public keys only.
3. **Four stages.** Each host keeps a state machine; a stage runs only from its predecessor and refuses if any file it
   would write exists. No configuration file, secret or handoff is overwritten, renamed over or deleted, except by
   `-discard-incomplete`, which removes only the files the dead run of the stage wrote (its journal holds the SHA-256 of
   each file's bytes). The command's own
   bookkeeping differs: `state.json` is replaced by a rename, and the journal and the stage's temporary directory are
   removed when a stage ends.

   | # | Run | Needs | Writes | Prints |
   |---|---|---|---|---|
   | 1 | `satbooth-setup public init` on the public host | the answers | SatBooth's secrets, the handoff `public-to-hub.json` | the pairing links (two with refunds), the handoff's digest |
   | 2 | `satbooth-setup hub init` on the hub host | the answers, `public-to-hub.json`, the digest stage 1 printed | the hub host's configuration and secrets, the bundle's env file, the copy of the wallet-rpc password to carry | the first start, the tunnel, "set the unlock password now", the wallet commands |
   | – | the operator, in the hub's page | the links of stage 1 | | copies the Wallet Public Keys (two with refunds, else one) |
   | 3 | `satbooth-setup hub pair` on the hub host | the Wallet Public Keys | `relay.json`, the handoff `hub-to-public.json` | its digest, the second start |
   | 4 | `satbooth-setup public finish` on the public host | `hub-to-public.json`, the digest stage 3 printed, with Monero the carried password file | `satbooth.json`, `wallet-rpc.pass`, the bundle's env file | the start command |

   The flags: `-dir` (the base directory, always), `-host-dir` (stages 1 and 2: the path of the base directory as the
   host sees it, which is the answer to `host_dir`), `-answers` (stages 1 and 2), `-hub-url` (stage 1: the hub's admin
   address as the operator will reach it through the tunnel, default `http://127.0.0.1:8080`, which only the links
   carry), `-handoff` and `-digest` (stages 2 and 4), `-invoice-wallet` and `-payout-wallet` (stage 3),
   `-wallet-rpc-pass` (stage 4), `-discard-incomplete` (any stage). An unknown flag, an extra argument and an explicit
   empty value of `-host-dir`, `-hub-url`, `-answers`, `-wallet-rpc-pass`, `-digest` or `-handoff` are usage errors (exit 2);
   `-digest ""` too (it never has an empty meaning); `-payout-wallet ""` means no payout key, `-invoice-wallet ""` is
   refused ("the invoice connection's Wallet Public Key is required"), and a flag left out makes the stage ask. Every refusal is exit 1. Everything that
   can be refused without an answer (a bad `-host-dir` or `-hub-url`, a wrong stage, a journal left behind, a directory
   in the way) is refused before the first question.
4. **What a handoff decides, and what it does not.** A handoff decides the NWC client public keys and SatBooth's
   `rpcauth` line (stage 2), the Wallet Public Keys (stage 4), whether the Monero files are written and the Monero
   login's user name. It decides no network and no address: both are typed on each host, the handoff's copies are only
   compared at stage 2, which refuses a mismatch, and no stage writes a network or an address read from a handoff. The
   pairing links are printed at stage 1, where the client keys are made: a link built from a key read out of a handoff
   could get a payout connection approved for another key. Stage 4 builds the relay's, bitcoind's and wallet-rpc's URLs
   from the hub address the operator gave at stage 1: a swapped URL would point a rail at a node that reports payments
   (threat model §9).
5. **The digest is checked by the command, before anything is written.** The stage that writes a handoff prints the
   SHA-256 of its bytes (eight groups of eight hex characters). The stage that reads it takes that digest from the
   operator (`-digest`, or a prompt that asks until the input has the shape of one) and refuses, before it writes
   anything, when the file's digest differs. The file holds no digest of itself. The handoff is read as a regular
   file (no symbolic link) of at most 4096 bytes and parsed strictly (one JSON object, every key once, no other key,
   version 1); a file that is empty or too large, malformed, of the other kind, of another version or holding a value
   that is not allowed is refused with one fixed message per reason; one that was edited within the rules is caught by
   the digest alone. A stage that reads a handoff keeps a copy of it in `<dir>/setup/` (`read-public-to-hub.json`,
   `read-hub-to-public.json`) as one of its journaled files. Stage 4 requires the handoff's payout wallet exactly when
   stage 1 made a payout key, and the carried password file exactly when stage 1's answers have the Monero rail (and
   that file must be mode 0600 and exactly 64 lowercase hex characters and a newline); stage 3 requires a payout
   Wallet Public Key exactly when the handoff of stage 2 had a payout client key, and refuses a wallet key equal to a
   client key or to the other wallet key.
6. **The answers.** One validation serves the file and the prompts: the answers file (at most 16 KiB, one object, no
   unknown or repeated key) and the prompts' answers go through the same parser, which refuses with an error that names
   the field and never repeats a value. The rules that matter:
   - a private-link address is an IPv4 literal that is not unspecified, multicast, link-local, reserved or the broadcast
     address, and loopback is accepted only on `regtest`; one function decides it for the answers and the handoffs. An
     IPv6 address is refused with "set up an IPv6 private link by hand (deploy/README.md)", because the bundles are
     qualified on IPv4 only. The two addresses differ, and neither lies inside the bundle's own Docker networks
     (`172.30.78.0/24` on the public host; `hub_subnet` and `electrum_subnet` on the hub host).
   - `public_addr` is an IPv4 loopback or unicast address, never `0.0.0.0`; `hub_admin_addr` is an IPv4 loopback or
     private address that differs from both link addresses and lies in neither Docker network; the hub's Docker
     networks are `/16` to `/29` and do not overlap, and `hub_ip` is a host address in its network that is not its first
     (the gateway) or its broadcast address.
   - **Refunds are never defaulted.** With approval keys, the four caps (`payout_budget_sat`, `payout_max_sat`,
     `payout_daily_sat` and `payout_alert_sat`, the last `0` for no alert) are asked whenever refunds are on and
     required in a file; without approval keys a cap key is refused, even as `0`. The bounds are those of the service's
     `payout` block, and a refund is at most the daily cap and the budget.
   - the descriptor and the Monero address are checked for shape by the rules of `service.ParseConfig`
     (`TestAcceptedAnswersAreAcceptedByService` holds the two together); a descriptor that holds a private key is
     refused. `monero_network` is `mainnet` only, because the hub bundle runs Monero on mainnet only (`monerod.conf`,
     the `monerod` command and `wallet-rpc.sh` carry no network flag): `stagenet`, `testnet` and `regtest` are refused
     with a message that points to the manual setup and to adding the `monero` block to `satbooth.json`, and the
     address is checked as a mainnet primary address. The prompt for it defaults to `mainnet`. Every value the answers
     do not decide is the example configuration's (`TestDefaultsMatchTheExample`), except `rates.margin_percent`, which
     is `"0"`, the documented default: a margin is the merchant's pricing decision.
   - the base directory is an absolute, clean path of letters, digits and `/._-` of at most 200 bytes, not `/`: it goes
     into a Compose env file. With `-answers`, the file's `host_dir` must be the `-host-dir` the command was given.
7. **The prompts.** One question per answer, in the order of the answers' struct, on standard error; the input is
   standard input, and its end before the last answer is an error. A default shows in brackets and an empty line takes
   it; `-` clears an optional answer. Questions that depend on each other form a group (the network and the two
   addresses; the Docker values; the Monero address and network; the approval keys and the four caps). After a group,
   and after a single question, the answers so far, with valid placeholders for the questions not yet asked, go through
   the real parser. A refusal that names an answer already asked prints the parser's reason and asks that answer's
   group again, the other answers kept; a second refusal of the same answer, or one the parser names across groups,
   asks every question again, with `[keep]` for the old answer: an empty line keeps it, and an answer is never
   displayed again. The Wallet Public Keys of stage 3 and the digests are asked the same way until they have the right
   shape.
8. **The three directories.** A host's files live under one base directory `<dir>` (the container's view of the
   answers' `host_dir`), on one filesystem:
   - `<dir>/setup/` holds `state.json`, `journal.json`, the handoffs written and read, `api-keys/<id>.key` on the public
     host and `carry/wallet-rpc.pass` on the hub host. No service mounts it. `state.json` holds the stage, the answers
     (the descriptor and the Monero address included, the operator's own) and the public values later stages need (the
     client public keys, the hub address), never a secret; it is read back strictly and revalidated with the parsers of
     the answers and the handoff.
   - On the public host, `<dir>/etc/` is `SATBOOTH_ETC`, `<dir>/backups/` is `SATBOOTH_BACKUPS`, and `<dir>/compose.env`
     is the bundle's env file.
   - On the hub host, `<dir>/config/` is `HUB_CONFIG_DIR`, `<dir>/wallet/` holds the Monero wallet files, and
     `<dir>/compose.env` is the bundle's env file.

   **The directory rules.** The base directory, `setup/` and every directory a stage writes into (`etc/` at stage 4,
   `config/` at stage 3) must be a real directory, never a symbolic link, owned by the user running the command, with
   mode exactly 0700; the command refuses otherwise. A first stage refuses when any directory it would create exists
   (an empty `setup/`, which a crash or a discard can leave, gets a message of its own). Every operation goes through
   one `os.Root` opened on the base directory, and the root is opened before the directory is checked, on the object
   that was opened, so no name resolves outside it and nothing is checked on one object and used on another. Every file
   is created with mode 0600 and every directory with 0700, whatever the umask. After the bundle's first start,
   `volume-init` gives files and directories to the services' users (`etc/` and `backups/` to 65532 on the public host,
   the hub host's configuration files and `wallet/` to their services), so stage 4 must come before the public host's
   first start.
9. **What a stage does when it commits.** It opens the root and checks the directories; it reads `state.json` and
   refuses a wrong predecessor; it refuses if an earlier run left a journal; it refuses if any target exists; it writes
   `setup/journal.json` (through a temporary name and a link, so a torn write never leaves a journal) naming the
   directories and files the stage may create with the SHA-256 of the exact bytes it will write to each, the temporary
   names, and the exact bytes of the `state.json` it replaces
   (`prev_state`, null for a first stage), and fsyncs; it writes every file into `<dir>/setup/tmp-<stage>/` with its
   final mode and fsyncs each; it links each into place (a link fails if the name exists, and works because the
   directory is on the same filesystem) and removes the temporary name; it writes the new `state.json` through
   `setup/state.json.tmp` and a rename and fsyncs the directory; it removes the journal. A name that appears after the
   check makes the run undo what it made and refuse. A stage run twice, or after a crash, therefore refuses, names the
   files in the way and changes nothing.
10. **`-discard-incomplete`.** It is bound to its stage: it reads `setup/journal.json`, accepts it only when it is a
    journal of that stage and host, that names only the directories and files the stage can create (its fixed list and
    at most 64 temporary names) each file with a 64-hex SHA-256, and whose `prev_state` is this host's state at the
    stage's predecessor (null exactly for a first stage), and otherwise removes nothing. It removes a journaled final file
    only if it exists and its content hashes to the journaled value (the command's own scratch, `state.json.tmp` and
    the files in the stage's temporary directory, goes whatever its content); a file under a journaled name with other content (a
    file the operator made after the run died, or one changed since) is kept and reported, never deleted. Then it
    removes the directories it names that are empty, puts back the exact `state.json` bytes the run replaced, and
    removes the journal last (a first stage's `setup/` goes with it). The commit's own undo, after a name appeared
    between the check and the link, follows the same rule. It never follows a symbolic link, and a lone
    `journal.json.tmp` (the run died before the journal was in place) is removed. An empty or unparsable journal is
    refused: the operator inspects it and deletes it by hand. `state.json` and `journal.json` are read with the same
    strict walk as the handoffs: one object, every key matched exactly and at most once, no unknown key, nothing after
    the object but white space, at most 64 KiB; the nested answers and handoff go through their own strict parsers.
11. **Files and commands, never secrets.** Setup writes files and prints commands. It opens no network connection, runs
    no other program and never prints a secret, a descriptor or a Monero address; the last two come from the answers
    file or a prompt, never from argv. Every generated secret is 32 bytes, written as 64 lowercase hex characters and a
    final newline (the `rpcauth` lines hold the salted HMAC of the password, as Bitcoin Core's own script computes it).
    Each `rpcwhitelist` line it writes into `bitcoin.conf` (two with Lightning only, three with the on-chain rail) is
    byte-identical to its line in `deploy/hub/config-example/bitcoin.conf`.
12. **What each stage prints** (besides the digest where the table says so):
    - Stage 1: the invoice link and, with refunds, the payout link, with "open them only after the hub's unlock
      password is set (stage 2)"; the digest; "copy `setup/public-to-hub.json` to the hub host"; where the plain API
      keys and the webhook secret are, for the merchant's application.
    - Stage 2: the first start (the hub only, with the env file's path); the SSH tunnel; "set the unlock password now,
      before anything else"; then the order to open the two links that stage 1 printed (it does not print them again:
      a link is never built here from a handoff); with the on-chain rail, the three `bitcoin-cli` commands of
      `deploy/README.md` (create the wallet, `getdescriptorinfo`, `importdescriptors`) with a placeholder for the
      descriptor; with Monero, the `monero-wallet-cli` command, which is where the operator types the view key (setup
      never asks for it) and which takes the wallet's password from `config/wallet-rpc/wallet.pass` (`--password-file`).
    - Stage 3: the digest; the second start; "copy `setup/hub-to-public.json`" and, with Monero,
      "`setup/carry/wallet-rpc.pass`, as a 0600 file" to the public host; "delete `setup/carry/wallet-rpc.pass` on this
      host once it is on the public host".
    - Stage 4: the start command; with Monero, "delete the carried password file from the channel you copied it
      through".
13. **The tools image and the wrapper.** The `satbooth-tools` target of `deploy/image/Dockerfile` holds
    `/satbooth-setup` and `/satbooth-pair` and nothing else (no entrypoint, no CA bundle). `deploy/setup.sh <dir> <host>
    <stage> [options]` builds it (`satbooth-tools:local`, from this repository, on every run: the Go build is offline,
    the modules being vendored, but the first build needs Docker to pull the pinned base image) and runs the one stage
    in a container with `--network none`, `--read-only`, `--cap-drop ALL`, `--security-opt no-new-privileges` and the
    calling user's numeric id, mounting only `<dir>` (read-write, as `/work`) and each file named by `-answers`,
    `-handoff` or `-wallet-rpc-pass` (read-only, at a fixed path). It creates `<dir>` with mode 0700 when it is missing,
    sets `-dir` and, for stages 1 and 2, `-host-dir` itself (a caller who gives either is refused), and refuses before
    it creates or mounts anything a path with a colon (Docker's mount syntax), a mounted file that is a symbolic link or
    not a regular file, and a repeated option. The report goes to standard output, the prompts and the image build to
    standard error.
14. **The gates.** `scripts/capslock.sh` compares the whole module with `docs/capslock.json` and then analyses
    `./cmd/satbooth-setup` and `./cmd/satbooth-pair` each alone, failing on a network capability, on a capslock failure
    and on an output without capabilities (the whole-module baseline lists the network for the package `setup` because
    of an over-approximation that cannot arise in the command alone, `docs/dependencies.md`). `scripts/image-check.sh`
    and `scripts/image-run-test.sh` check the tools image's files and its refusal without arguments. The `deploy` group
    of `scripts/qualify.sh` starts both bundles from what `deploy/setup.sh` writes, not from files the test writes:
    stages 1 and 4 for the public host (both connections paired from stage 1's client keys, the generated `payout`
    block in use), stages 2 and 3 for the hub host (a stage 1 of its own makes the handoff), with a refused mismatch
    (`qualify/REPORT.md`, "Deployment bundles").

### Several webhook endpoints, as built (plan 13)

This subsection replaces the single `webhook` setting described in the earlier subsections: where they say
`webhook.url` or the webhook secret, read each endpoint's. Built as written below. The threat model's §11
("Several webhook endpoints (plan 13)") carries the security rules.

A merchant often wants the same events in more than one place: the shop, an accounting tool, a logger. The rules:

1. **One form.** `webhooks` is a list of 1 to 8 endpoints, each `{"name", "url", "secret_file"}`. It replaces
   `webhook`: there is one form, and a configuration with the old key is refused with a message that names the new
   one.
2. **Every event goes to every endpoint.** There is no per-endpoint filter.
3. **Each endpoint is delivered on its own.** It has its own attempts, retries, 24 hour limit and "abandoned" line,
   which names the endpoint. A dead endpoint never delays another.
4. **Each endpoint has its own secret.** No two endpoints share one, and none shares the alert receiver's.
5. **A new endpoint catches up by itself.** An endpoint that is new to a data directory receives the events of the
   last 24 hours that it has not been sent (the retry window). A removed endpoint is no longer delivered to, and its
   recorded attempts stay. A renamed endpoint is a new endpoint.
6. **The signature and the body are unchanged** (`SatBooth-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>`), so a
   receiver written for the single webhook keeps working.
7. **The operator's `alerts` receiver stays single.**
8. **The setup command** keeps asking for one webhook URL and writes it as the endpoint `main`, with a generated
   secret of its own. More endpoints are added in `satbooth.json`, each with a secret file of its own.
9. **The store's attempts gain the endpoint's name** in a new migration; attempts recorded before it belong to
   `main`.

From these follow the rules below, which were built as written unless "as built" says otherwise.

- **Names.** An endpoint's name is 1 to 64 characters of `[A-Za-z0-9._-]`, the store's name rule, and is unique in the
  list. As built, the deliverers' log lines and the operator alert name an endpoint by its name, and
  `webhook.NewEventDeliverers` names it by its name too, or by its position (`endpoints[2]`) when the name itself is
  bad. The held-back line and its alert carry no `endpoint` (one per event). A configuration error names an entry by
  its position in the list, counted from 0 (`webhooks[2].url`), even when the entry has a name. Nothing shows a URL or
  a secret.
- **The store remembers each endpoint's first pass.** A row per name, written once, holds the time of the endpoint's
  first delivery pass in this data directory, recorded when its deliverer first runs. An endpoint is listed the events
  created at or after 24 hours before that time (and, as before, not older than the retry window plus seven days; as
  built, the clock caps the bound, below). So
  a new endpoint catches up with the last 24 hours and not with a week of old events that would each log an abandoned
  line, and no line is logged for an event older than that bound.
- **`main`.** The endpoint that existed before the change is `main`. A store with events gets the row for `main` at the
  oldest event's time when the migration runs, so `main` continues exactly as the single webhook did. In a store
  without events `main`, like any name, gets its row at its first delivery pass.
- **A name removed and added again is the same endpoint.** It continues where it stopped, under the rules after
  downtime: what it was sent is not sent again, what it abandoned stays abandoned, and an event it never tried gets its
  attempts (one last attempt, if the event is older than 24 hours).
- **What a start does with a changed list.** A name that is new to the data directory: it is delivered the events of
  its window, as above. A name that was removed: no deliverer runs for it, and its recorded attempts stay. A name added
  again: as in the previous rule. A renamed endpoint: the old name is a removed one and the new name a new one.
- **Secrets and URLs.** The cross-endpoint and alert secret comparisons are made on the secrets as HMAC keys (a secret
  that is another followed by zero bytes is the same key and is refused too). The `alerts` URL must differ from every
  endpoint's URL.
- **Alerts.** The abandoned alert carries the endpoint: the alert allowlist gains the key `endpoint`, so two endpoints
  that abandon one event are two alerts and not one alert with a repeat. "A webhook event is held back" (a malformed
  stored quote) is about the event: one line and one alert per event, whatever the number of endpoints.

The plan-10 alert deliverer is unchanged: it is a further instance of the deliverer over the alert tables, with the
alert secret.

As built, beyond the text above:

- **The configuration.** `Config.Webhooks` is a list of `WebhookEndpoint` (`name`, `url`, `secret_file`);
  `MaxWebhookEndpoints` is 8. `ParseConfig` refuses the key `webhook`, with or without `webhooks` beside it, with
  `config: webhook is replaced by webhooks (a list of endpoints)`, before it reads the value. A configuration message
  names an entry by its position counted from 0 (`webhooks[0]` is the first entry), never by a URL. Run checks the count
  again, since a configuration built in code skips `ParseConfig`. The secrets are loaded and compared as HMAC keys
  before the data directory is touched, so equal secrets, or one equal to the alert secret, stop the start with the
  reason `webhook_secret_file` or `alerts_secret_file`.
- **One worker per endpoint.** `webhook.NewEventDeliverers` makes one deliverer per endpoint, and each runs in a worker
  of its own, so a dead or slow endpoint holds up its own requests only. The worker limit is 16: the 8 workers of the
  other loops plus `MaxWebhookEndpoints`. A test pins the number and starts Run with every rail, the payout, the alerts
  and eight endpoints.
- **The first-pass bound is capped by the current clock.** The first pass is written once, by the clock of the moment,
  so a clock that ran ahead would stamp a time in the future and hide events that are inside their 24 hours by the
  current clock. An endpoint is therefore admitted the events created at or after the later of `now − 24 h − 7 d` (the
  retry window plus the seven days) and `min(first pass, now) − 24 h`. An event that already has an attempt for the
  endpoint stays listed until it is delivered or abandoned, whatever the floor, so the floor never takes away the last
  attempt or the abandoned line of an event the endpoint has tried. Two residuals remain: an event the endpoint never
  tried, when the clock ran ahead at the first pass and the service was down (or the event otherwise had no attempt)
  until the floor passed it, gets no attempt and no line for that endpoint; and a clock that ran behind at the first
  pass lets a new endpoint be sent more than 24 hours of events, up to the seven-day bound, with an abandoned line for
  each that fails. The threat model's §11, item 4, states them.
- **The store.** Migration 9 adds the table of first passes, rebuilds the attempts table with the endpoint's name (the
  existing attempts belong to `main`) and, in a store with events, writes the row of `main` at the oldest event's time.
  Both tables keep the `immutable` triggers.
- **The abandoned alert** carries the key `endpoint`. The line "a webhook event is held back" does not: it is one line
  and one alert per event.
- **The setup command** writes `webhooks` with one entry, `main`, whose secret file is `etc/webhook.secret`.

### The final fixes, as built (plan 14)

Four changes to existing behaviour and one procedure that is documentation only. The threat model's §3 (the on-chain
count, the hub's network), §4 (restoring a backup) and §11 (the wake-up) carry the security rules. The changes:

1. **An on-chain payment's confirmations are capped by the count bitcoind returns for the transaction,** as the Monero
   rail does.
2. **The restore procedure is documentation, and one rule:** before the first start after a restore, replace
   `payout.approval_keys` with new keys and keep the `payout` block; approve no refund with a new key until the payouts
   made after the backup are reconciled by hand. (A start without the `payout` block is refused when the restored
   store holds an unfinished refund, and it adds nothing to replacing the keys.) When bitcoind's wallet is rebuilt as
   well, its descriptor is imported with a next index well above anything issued since the backup.
3. **SatBooth refuses to start if the hub reports another network than `nwc.network`.**
4. **The webhook deliverers are woken when an event is written;** they no longer scan the store every second.
5. **A shop runs on mainnet.** `testnet`, `signet` and `regtest` stay supported for development work: they get no
   special handling, and nothing is built around them for a merchant.
6. **A late output past its order's watch window is read again before it is recorded; nothing is recorded without a
   reading.**

From these follow the rules below, as built.

- **The on-chain bound is kept in memory, for one pass.** The defect: a pass reads the chain tip, stores it with the
  scan position, and only then reads each transaction with `gettransaction`. If the tip goes down in between (a reorg
  onto fewer blocks, or a bitcoind that was killed right after blocks were mined and came back with a shorter chain),
  the count taken from the stored tip is higher than the transaction's real confirmations, so a payment could be
  recorded as paid too early, and an output could be settled for good (never polled again) at less than the settled
  depth. The rule: nothing on-chain is recorded, as paid or as late, or settled for good, without a `gettransaction`
  reading in the same pass. A mined output counts at most the `confirmations` of the last completed pass's answer for
  its transaction, and only when the answer's block, by height and block hash, is the block its mark names; a mined
  output that is not yet recorded and has no such reading counts zero (on a tier of 0 a zero count does not stop a
  record; there every unsettled on-time output is read in the pass or its order is skipped, and a late output needs at
  least 1 confirmation). The rule is applied where an order's outputs are
  loaded, `orderViews` (`onchain/settle.go`), through one helper, `boundOut` (`onchain/watch.go`); `settled`
  (`onchain/watch.go`), the permanent settle, uses the same helper on the mark it loads. So the paid decision, the
  permanent settle and the count the API, the checkout page and the status show (`rail`, `onchain/rail.go`) are one
  count. A recorded output's count is not bounded: it decides nothing (a reversal reads the mark only), and a settled
  output is not read again, so its shown count goes on growing. `track` takes the observation (height, block hash and
  `confirmations`) from this pass's `gettransaction` answer that gave the output's mark, whether it changed the mark or
  confirmed it (when that answer showed no confirmation and the output is read again, from the second answer);
  `trackAll` publishes the pass's observations, as one map that is never written afterwards, when its tracking step
  completes, before the pass settles; a pass that ends during discovery or tracking publishes nothing, and the last
  published observations stay. The Monero rail takes the same counts from wallet-rpc's `confirmations`, which it
  stores; SatBooth stores nothing here, so there is no schema change: a restart reads again before it settles anything.
  The watched outputs are read every pass, as before. A late output (first seen at or after its order's expiry) of an
  order past its watch window is not watched. A pass settles such an order only when it was touched (a new payment to
  its address, or an on-time output still followed), and before it settles, `trackAll` reads again each late output of
  that order whose current mark is in the mempool or mined, that is not recorded and not foreign (`work`,
  `onchain/watch.go`). It is then recorded late from that reading, or not at all when the transaction is gone: its mark
  becomes dead and no `order.late` is sent. A late payment still in the mempool when its window closes, and mined later,
  is recorded late (`order.late` and its `ALERT`) when its order is next touched and it has its tier's confirmations; a
  dead one is not read again. An output with no mark (a payment that arrived after the window) is not read and not
  recorded. The settle orders of `OnchainOrdersPastExpiry` and `OrdersAwaitingPaidAgain` are inside the
  window, where every unsettled output is watched; a test pins it. Cost: no extra bitcoind call in the normal case; for
  such a late output one `gettransaction` a pass in which its order is touched (two calls with `getmempoolentry` while
  it is unconfirmed, six in the pass that finds it gone, as for the stuck set), until it is recorded and settled or
  dead; the local cost is one `orderViews` per touched past-window order a pass. If that read is refused or answers
  badly, the output is not recorded in that pass, its order is in the pass's stale set and nothing else (no skip), and
  the order's on-time payments are still recorded; while the mempool is still loading the output is not recorded in
  that pass and the order is not marked stale (`errLoading`); a transport failure ends the whole pass, which records
  nothing. A dead late output ends with a settled fact once its order is final and logs no `ALERT`
  (the left-pool `ALERT` is for an on-time output). What stays: a reorg deeper than the settled depth after an output
  was settled (Ruling G); a late output of a past-window order that is never touched again, which is never read and
  never recorded; a late payment that arrived after the window, which is not recorded (the after-window `ALERT` names
  its order); an unrecorded output, which shows zero confirmations until a pass has read it (after a start, or when its
  read failed, and then the order reads stale); and a settled or recorded output not read in the last pass, which shows
  the scan position's count. A check of the tip at the end of the pass would not be enough: a drop and a return to a
  higher tip between the two reads leaves it unchanged.
- **The hub's network is checked at start,** for the invoice connection and, when the `payout` block exists, for the
  payout connection, from the `network` of each `get_info` answer. The two must name `nwc.network` (and
  `payout.network`, which equals it). `checkNetwork` (`service/service.go`) runs last in the invoice connection's check,
  and in the payout connection's after the methods checks and before the budget check, from the same `get_info` answer
  (no extra call), and before the store's network is recorded, so a refused start records no network. Its three refusals
  are: the connection's
  `get_info` "does not state the network" (with the instruction to pair it with `satbooth-pair -connection <invoice or
  payout>`); the hub "is on the network" `X`, "not nwc.network" `Y`, when the hub names one of the four known names; and
  the hub "reports a network SatBooth does not know", when it names any other string, which is never printed. An answer
  that names no network is refused: Alby Hub states its network only to a connection that holds the `get_info`
  permission, so both pairing links ask for it, the hub's form shows it ticked as "Read your node info", and an
  operator who unticks it is refused at start with a message that says how to pair. The pairing tools say to leave that
  permission ticked. The pairing discloses the node's alias, colour, public key, network, block height and block hash
  to the connection and adds no way to move money; the public key is in every invoice. The per-invoice check of every
  `make_invoice` answer stays. The start-up check makes the sentence "a
  wrong `nwc.network` still starts and records it" false: it no longer starts when the hub states its network, and a hub
  that states none is refused. A shop runs on mainnet; `testnet`, `signet` and `regtest` stay supported for
  development. The setup command writes mainnet, signet and regtest setups and leaves testnet to be set up by hand.
- **The wake-up is a hint; the store is the truth.** The store wakes the event deliverers after each event it
  inserts, and the alert deliverer after each new alert row, once the write has ended: `AddEvent` and `AddAlert`
  (`store/events.go`, `store/alerts.go`) raise it after the write function has returned, so the transaction is
  committed or rolled back and the store's write lock is released, and only when the write ran an insert (a failed
  commit then costs one pass; not for a duplicate event, an alert that was a hit on an existing row, a repeat of a
  delivered alert or a day that is full). A wake-up tells a deliverer when to look, never what to send: the deliverer
  lists what is undelivered from the store, as before. Each deliverer also passes at its next retry or deadline,
  whichever is due first, and after a fallback of 30 seconds, so a wake-up that is missed costs at most 30 seconds of
  idle waiting, counted from the end of the last pass (`fallbackInterval` in `webhook/deliver.go`); the starts of two
  passes are at least one second apart (`pollInterval`, as before). When it is woken the store is read; when nothing is
  waiting it is read every 30 seconds, not every second. Measured on a developer's machine (Apple M1 Max, the real
  SQLite driver), listing the undelivered events with nothing pending, 23,000 delivered events and 8 endpoints, takes
  about 26 ms (`BenchmarkUndeliveredEvents` in `store/`); the figure is a measurement, not a guarantee. An event stored
  while a pass is running is picked up by the next pass. A pass lasts as long as its requests, which go one at a time,
  each with its 10 second timeout, so a slow receiver delays its own later events and no other endpoint's. A held-back
  event does not set the next time to look. The retry steps, the 24-hour limit, the abandoned line, the first-pass
  admission and every request and log line are unchanged. The signal is a channel of one slot per deliverer that never
  blocks the writer and is in memory only; one process owns a data directory, so every event written while the
  deliverers run is written by that process.
- **Restoring a backup while refunds are in use is a procedure,** in `docs/backup-and-restore.md` ("Backup and restore")
  and `deploy/README.md` ("Restore"), and the threat model's §4 states why and what it leaves: payouts made after the
  backup are never learned by the restored store, and a rebuilt wallet's next index is set above everything issued since
  the backup (bitcoind by its import, Monero by making subaddresses). No code changes.

## 4. Quality bar

- Every parser and normaliser (descriptors, amounts, the claimant-invoice syntax check, Nostr event
  serialisation, webhook signatures) gets seeded property/fuzz tests whose inputs cross every boundary.
- Never hand-write bech32/BOLT11 decoding. SatBooth decodes no BOLT11 data field: it checks a claimant
  invoice's syntax and reads the amount in its human-readable part, the hub enforces amount, expiry and
  duplicates (`dependencies.md` §4), and bitcoind derives addresses.
- An independent code review before any change reaches a running instance.
- **Test environment:** unit tests, vectors and fuzzing run anywhere with no network.
  Tests that need nodes run against a dev-only Docker Compose setup in this repo, never in production:
  bitcoind on regtest, two Alby Hub containers pinned to the qualified version (one is SatBooth's hub, the
  other plays the merchant's node and the customer wallet), SatBooth's relay, and monerod on regtest
  with monero-wallet-rpc. Every image is pinned by digest. A one-time mainnet smoke test with tiny amounts
  (§2.1) is the operator's, after the Lightning payment path works.
- **As built (plan 4, `dev/regtest/`, `qualify/`, `scripts/qualify.sh`; `docs/development.md` has the how-to).**
  SatBooth makes its own pairing key (§2.1); bitcoind is built in this
  repo from the Bitcoin Core 32.1 release tarball, checked against a pinned SHA-256 on a digest-pinned Debian
  base, with no third-party image; the suite runs locally and in a separate GitHub workflow (manual dispatch
  and weekly), while normal pull requests keep the offline checks. Implementation choices:
  - the qualified hub is the v1.24.6 release image, pinned by digest (`dependencies.md` §4); the tag points at
    `921003b`, the commit the source citations name;
  - the Compose network is `internal: true`, so the test network has no internet and the run is also the egress
    test of §2.1. The tests run in a container on that network, never on the host. Observed: the hubs reach
    `running` without internet, and the startup channel-suggestion call fails without blocking;
  - each hub has its own `satbooth-relay` sharing the hub's network namespace, because the relay needs its hub
    listener on loopback and its client listener on a specific IP. The merchant's hub needs NWC too: hold
    invoices exist only over NWC;
  - the measured values are in `qualify/REPORT.md`.
- **As built (plan 5).** Every parser and validator has a seeded property test and a fuzz target: `qr` (`FuzzSVG`),
  `store` (`FuzzValidators`), `limits` (`FuzzConfig`), `lnpay` (`FuzzPaidTransferID`), `webhook` (`FuzzVerify`), `api`
  (`FuzzCreateOrder`), `checkout` (`FuzzPath`) and `service` (`FuzzConfig`). The money paths are tested with a fake
  wallet for replay (a payment found twice, concurrently), double pay, timeouts, lost responses and crashes at every
  step of "pay" and of the reconciler. `qualify/` runs the whole service against real hubs: a payment, a replay, an
  expiry, a restart with the customer paying meanwhile, a lost relay, and a relay connection cut under a running
  service. `qualify/REPORT.md` ("Lightning end to end") says what each phase proves and what it does not. The full
  script takes about 20 minutes, because the expiry phase waits in real time.
- **As built (plan 6).** New fuzz targets: `btcrpc` (`FuzzParseBTC`, `FuzzParseSignedBTC`, `FuzzDecode` over the
  envelope and every decoder, `FuzzLoadPassword`, `FuzzValidators`), `descriptor` (`FuzzParse`, `FuzzDecodeCheck`),
  `onchain` (`FuzzHDKeyPath`, `FuzzTransferIDs`) and `checkout` (`FuzzPaymentURI`, `FuzzRender`); the existing
  `store.FuzzValidators`, `api.FuzzCreateOrder` and `service.FuzzConfig` cover the addresses, `onchain_sat` and the
  `onchain` block. The watcher is tested against `onchain/onchaintest`, an in-memory bitcoind that derives its answers
  as Core 32.1 does, for replay, double pay (two outputs, both rails), RBF, reorgs before and
  after paid, partial, over- and late payments, the window's end, an outage of each method, restarts and a crash at every
  step of the address and of the pass. `qualify/` runs the whole service against the regtest bitcoind through the whitelisted user: one
  order per tier, RBF, reorgs, partial and late payments, a restart, the index after `SIGKILL`s of bitcoind and a
  re-import without `next_index`, and the whitelist itself (`qualify/REPORT.md`, "On-chain end to end"). The phone
  scan of the unified QR is a manual step, not done yet.
- **As built (plan 6.1).** No new fuzz target: `limits.FuzzConfig` draws the new `max_open_per_account` and checks
  its default and bounds, and `service.FuzzConfig` parses whole configurations, the new key and a tier of 0 included;
  the tier rules have a seeded property test (`onchain.TestTiersProperty`), and a tier without its `confirmations`
  key is a table test (`service.TestTierConfirmationsRequired`). The inheritance rule has a seeded
  property test of its on-time invariant; the stuck set, 0-conf, the re-read before `dead` and the both-rails rule are
  tested against `onchain/onchaintest`, with crash scenarios at each step. `qualify/` adds a 0-conf payment and its
  double spend, a bump after the expiry, a stuck payment past its window (its own environment, on a shifted clock) and
  the per-account cap (`qualify/REPORT.md`, "Plan 6.1 follow-ups").
- **As built (plan 7).** New fuzz targets in `rates`: `FuzzParseDecimal`, `FuzzParsePercent`, `FuzzSats` and one
  per provider parser (`FuzzParseKraken`, `FuzzParseCoinbase`, `FuzzParseYadio`, `FuzzParseCustom`), each beside a
  seeded property test; the existing `store.FuzzValidators` (the quote), `api.FuzzCreateOrder` (`price`, `currency`,
  `rails`), `checkout.FuzzRender` (the price line; the Yadio credit is the table test `checkout.TestYadioCredit`) and `service.FuzzConfig` (the `rates` block)
  are extended. `rates.TestNoFloatInMoneyCode` scans the sources of `rates`, `api`, `store` and `checkout`, their
  subpackages included (but not `*test` directories), for float types, literals and conversions (`TestNoFloatScanCatches` proves it catches
  them); the median, the spread rule and freshness are tested exactly at every boundary, and the fetcher and its
  cache under `-race` with stalled, failing and lying providers; the client's rules against `rates/ratestest`. The money paths are
  tested for a repeat at a moved rate (one order, the first quote), concurrent creations, a restart (the quote read
  back unchanged) and a store failure mid-transaction (no order without its quote). The recorded provider answers
  are checked offline against each other (`TestFixturesAgree`). `qualify/` adds `TestFiatPricing`, `TestFiatOutage`
  and `TestOnchainFiat` against the fake rate server on regtest: a priced order paid on each rail, the rate locked
  across a restart, providers failing, disagreeing and going stale, and the `ALERT` lines (*observed*:
  `qualify/REPORT.md`, plan 7, "Fiat pricing (plan 7)").
- **As built (plan 8).** New fuzz targets: `xmrrpc` (`FuzzParseChallenge`, `FuzzTransfers`, `FuzzValidAddress`,
  `FuzzEnvelope`), `monero` (`FuzzTransferIDs`, `FuzzConfigValidate`), `rates` (`FuzzConvert`, `FuzzParseBitfinex`) and
  `checkout` (`FuzzMoneroURI`), each beside a seeded property test; `rail` adds none (`FuzzTiers` and
  `TestTiersProperty` moved there from `onchain` with the tiers). The existing `store.FuzzValidators` (the subaddress,
  the Monero mark and the piconero bound), `api.FuzzCreateOrder` (`monero_piconero` and `rails` naming `monero`),
  `checkout.FuzzRender` (the rail picker and the Monero fields) and `service.FuzzConfig` (the `monero` block) are
  extended. `rates.TestNoFloatInMoneyCode` also scans `rail`, `xmrrpc` and `monero`. The moved rules keep `onchain`'s
  tests unchanged. The Monero money paths are tested against `monero/monerotest`, an in-memory wallet-rpc that answers
  as v0.18.5.4 does, for replay, double pay, timeouts, restarts and a crash at every step of the subaddress and of the
  pass, reorgs, pool drops, 0-conf and `double_spend_seen`, locked, partial, over- and late payments, the window's end,
  a stale peer and the pause on a wrong wallet. `qualify/` adds sixteen Monero phases against a regtest monerod and a
  view-only wallet-rpc behind a Digest login (`qualify/REPORT.md`, "Monero end to end"): setup (the codes inferred from
  the source, read on the real wallet-rpc), pay at each tier, partial, a pool drop and 0-conf, a reorg, the refused
  `unlock_time`, a restart, a killed and restored wallet file, late, a stuck payment past its window on a shifted clock,
  the XMR rate on the fake rate server, and both rails. Not on regtest: `double_spend_seen` and a locked payment (no
  such transaction can be built through wallet-rpc), the pause under a running service, and two Digest clients against
  one wallet-rpc. The phone-wallet scan of the Monero QR is a manual step, not done yet.
- "Simpler is always better": every dependency and every feature has to earn its place.

## 5. Design decisions

1. **Relay:** SatBooth's own small relay, written in this repo (§2.4) in Go and structured after khatru (public
   domain; no code copied). Its tests were written from the NIPs (nostr-protocol/nips at 400d157) and from the
   hub's source (getAlby/hub at 921003b, getAlby/go-nostr at 45060cc).
2. **Runtime:** Go, self-hosted. The dependency choices that follow from it are in `dependencies.md` §6.
3. **On-chain detector:** a bitcoind watch-only wallet.
4. **Hub backups:** an encrypted backup of the hub's data directory plus its static channel backup. SatBooth
   does not patch third-party nodes, and running backups is the operator's job; these docs only state the
   requirement.
5. **Payout approval:** an Ed25519 approval from the admin system for the amount. The customer attaches the
   invoice later, and each approval can be claimed once (threat model §4).
6. **Expired invoices in the hub:** the hub (at 921003b) never deletes a transaction row, and an unpaid invoice
   stays `PENDING`. SatBooth's global invoice caps bound the growth; purging them is left to the operator.
7. **License:** Apache-2.0 (`LICENSE`). Every dependency and source is compatible with it: ISC, BSD-3, MIT and
   public domain (`dependencies.md` §3), BSD-2-Clause (BIP-173's test vectors and checksum design) and CC-BY-4.0
   (BOLT 11's example invoices, used as test vectors); `ACKNOWLEDGEMENTS.md` lists each.
8. **Payout invoices:** `lookup_invoice` finds the hub's own invoices and also the payments it made: on the payout
   connection a lookup of an invoice the hub already paid returns its outgoing row (`qualify/REPORT.md`,
   `TestRefundHubKnown`), which is how a refund to an invoice the hub knows ends `invoice_known`. The hub's
   `pay_invoice` refuses an `amount` that differs from the invoice's own amount, refuses expired invoices and
   refuses a second payment while one is pending or settled. So SatBooth always sends `amount` and needs no BOLT11
   library. This is hub behaviour beyond NIP-47, pinned and tested per hub version (`dependencies.md` §4).
