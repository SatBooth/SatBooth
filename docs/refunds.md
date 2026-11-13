# Refunds

SatBooth pays Lightning refunds of orders paid over Lightning, in msat, and only with a person's approval. A person
creates the refund in the merchant's admin system, which signs an approval with an Ed25519 key that never touches
the SatBooth host (`satbooth-approve`, or the same format in its own code: `docs/threat-model.md` §4 has the byte
layouts). The customer then gives a Lightning invoice in the merchant's app, which claims the refund with the approval
and the invoice. On-chain and Monero refunds are paid by a person from the merchant's own wallet; SatBooth pays none
of them.

**The operator's steps.**

1. Pair the payout connection: `satbooth-pair -key <path> -hub <url> -connection payout -max-amount-sat <n>`. It asks
   for `pay_invoice` and `lookup_invoice`, and `get_info` ("Read your node info": leave it ticked, as the hub states its
   network only to a connection that holds it); do not add any other method in the hub (SatBooth refuses to start
   with one).
   The hub also grants `pay_keysend`, `multi_pay_invoice` and `multi_pay_keysend` with `pay_invoice`, and the methods
   `get_info` and `get_budget` to every connection (its network comes only with the `get_info` permission asked for
   above); SatBooth tolerates those and never calls the first three.
2. Set the connection's **lifetime budget** in the hub (renewal "never"; SatBooth refuses one that renews). It is the
   hard cap on everything refunds can ever pay, fees included: keep it small and raise it by hand when it runs low.
   `budget_alert_msat` warns you first.
3. Add the payout connection to the relay as a pair of its own, and fill in the `payout` block.
4. Make the approval key pair on the admin system (`satbooth-approve keygen`), put the public key in
   `payout.approval_keys`, and give the admin system its key file. **Keep the signing key off the SatBooth host**: a
   key on the server would let a compromised server approve its own refunds.
5. Make two API keys for the merchant's app, one with `refunds:write` and one with `refunds:read`.

**Do not restart the hub while a refund is in flight.** A hub shut down mid-payment can mark the payment `failed` and
still settle it after its restart. SatBooth then shows the refund `failed`, and a new approval would pay it a second
time. If the hub was restarted, check the hub's own record of the payment before approving a failed refund again.

**Do not delete, replace or re-pair the payout connection while any refund is `claimed` or `sent`.** SatBooth does
not record which connection sent a refund, and a new connection may not see the old one's payments: a lookup that
reads "no record" could then lead a person to void a refund that was paid.

**Keep SatBooth's host and the relay's host on NTP time, and never step either clock by hand while a refund is
`sent`:** the margin between a refund request's expiration and the void wait (11 minutes less `timeout_seconds`,
at least 1 minute) assumes their clocks agree within it.

`POST /v1/refunds` (key with `refunds:write`) claims a refund:

    {"approval": "<296 lowercase hex characters>", "invoice": "lnbc..."}

The invoice is lowercase, without `lightning:`, for the configured network, and must carry exactly the approved
amount: an invoice without an amount, or with another amount, is refused at claim (`invalid_invoice`). The answer
is 202 with the refund view; the refund is then sent in the background.
The sender runs on every claim and every 5 seconds. It first ends, from SatBooth's own records, each claimed refund
whose approval expired (`expired_unsent`) or whose key was removed (`key_revoked`). Then it reads the hub budget once:
a refund whose amount plus the larger of 1 % and 10 sats is not left gets an `ALERT` and waits, without holding back
the others. Of the refunds the budget covers it takes the oldest whose invoice lookup has not just failed (a refund
the hub keeps failing on is tried again once the others have had their turn), looks the invoice up (a hub that
already knows it ends the refund `invoice_known`; an answer with another invoice gets an `ALERT` and is treated as a
failed lookup), and sends it. One refund is sent per round, so N claimed refunds
take about N × 5 seconds. A round makes at most 2 hub calls before its one `pay_invoice` (the budget read and one
lookup), and 1 when the budget read fails or the budget covers none.
`GET /v1/refunds/{refund_id}` (key with `refunds:read`) reads it; poll it, there are no refund webhooks.
`POST /v1/refunds/{refund_id}/void` (key with `refunds:write`) ends an open refund (see "Voiding a refund" below):

    {"approval": "<240 lowercase hex characters: a void approval for this refund id>"}

The refund view:

    {"id": "<32 hex>", "order_id": "<32 hex>", "amount_msat": 21000000, "state": "failed", "reason": "voided",
     "claimed_at": "<RFC 3339>", "resolved_at": "<RFC 3339>"}

- `state`: `claimed` (admitted, not sent yet), `sent` (the payment was sent once; SatBooth only looks it up from
  now on), `settled` (paid) or `failed`;
- `reason`, only when `failed`: `expired_unsent` (the approval expired before a send), `key_revoked` (its key was
  removed from `approval_keys` before a send), `invoice_known` (the hub already knew the invoice; nothing was sent),
  `not_sent` (the relay refused the request), `failed` (the hub reported the payment failed), `cancelled` (a person
  voided it while it was still `claimed`; nothing was sent), `voided` (a person voided it after it was `sent`, and
  the hub had no record of it or showed it `expired`) or `recovered` (resolved from the ledger at start-up);
- `resolved_at`, only when `settled` or `failed`. Times are UTC, in whole seconds.

A `failed` refund does not count against the order or the daily cap, and its approval is spent: paying again needs
a new approval. The view never carries the invoice, a payment hash or a preimage.

**Voiding a refund.** A person ends an open refund with a signed void approval (`satbooth-approve void`). A void
ends any refund that is `claimed` or `sent`, except one the hub reports `pending`, which waits until the hub settles
or fails it, and one the hub answers about in a way that does not fit the refund (`hub_unavailable`, with an `ALERT`). A
successful void answers 200 with the refund view. What it does depends on where the refund is:

- **Still `claimed`** (waiting for its turn, for budget, or held by an `ALERT`): SatBooth has sent nothing, so the void
  makes no hub call. The refund ends `failed` with the reason `cancelled`, and its amount is free again on the order
  and the daily cap. SatBooth moves its ledger first: it reserves the amount in the ledger if it had not and releases
  the reservation, both in one ledger write applied whole or not at all, so a restart finds the refund ended
  (`failed`, `recovered`) and never sends it.
- **`sent`:** a void is accepted from 11 minutes after the send (a refund's request expires at its send time plus
  `timeout_seconds`, at most 10 minutes, so the 11-minute void wait always outlasts it and a relay whose clock agrees
  with SatBooth's no longer serves the request) and decides with a fresh lookup at the hub: no record or `expired`
  fails the refund `voided`; a payment the hub shows `settled` or `failed` resolves the refund that way; `pending`
  answers `refund_in_flight` and changes nothing. A refund that stays `sent` with an `ALERT` saying the hub has no
  record of it (or shows it `expired`) never resolves by itself: check the hub, then void it.
- **The store and the ledger disagree** (rare: a write that was lost, or a database changed by hand). SatBooth
  follows the record that kept its data, plus the hub's fresh answer, and never reverses a ledger entry:
  - a `claimed` refund whose payment the ledger already shows paid: the void settles the refund, because the ledger
    is the record of the payment; nothing is sent and the ledger is not touched. SatBooth reports such a refund at
    start-up with an `ALERT` saying the ledger shows a refund paid that has no send marker.
  - a `sent` refund with no ledger reservation: the void waits the same 11 minutes and asks the hub. If the hub says
    `settled`, SatBooth writes the ledger entry the refund would have had and settles it; `failed`, no record or
    `expired` fail it as above, with no ledger entry. SatBooth reports such a refund with an `ALERT` saying a sent
    refund has no pending ledger transfer once the hub answers settled or failed for it.

A void never marks paid a refund that no record shows as paid. A void that SatBooth cannot complete answers 500
`internal` and leaves the refund `claimed` or `sent`: its ledger refused the entry (an `ALERT`), its store refused the
result (an `ALERT`, for example after the clock was stepped back), or a store or ledger call failed. A result the
store refused is written again on SatBooth's next rounds, and a second void meanwhile answers `invalid_state`. After a
restart, a refund whose reservation the void had released ends `failed` (`recovered`). The one cancel that a restart
undoes is a `claimed` refund whose reservation the ledger refused (the merchant's account holds less than the refund)
and whose result was refused: read the refund again and, if it is still open, void it again. A refund whose ledger
already shows it resolved but that has no result yet is refused `invalid_state` with an `ALERT`: restart SatBooth, and
start-up recovery ends it.

**The `ALERT` lines that ask a person to void**, and what to do:

- "a sent refund has no pending ledger transfer; nothing changed, a person voids it": check the hub's record of the
  payment, then void it once the 11 minutes have passed; the hub's answer decides.
- "the ledger shows a refund paid that has no send marker; nothing changed, a person voids it": void it; it ends
  `settled`.
- "the ledger shows a refund resolved that has no result; nothing is sent, restart SatBooth to finish it or void it if
  the ledger shows it paid" (the sender's line), and "a void was refused because the ledger already resolved the
  refund; nothing changed, restart SatBooth to finish it" (a void's line): restart SatBooth. A refund whose ledger
  reservation was released then ends `failed` (`recovered`); one whose ledger shows it paid gets the line above at
  the restart, and a void settles it.
- "the hub has no record of a sent refund; a person decides and voids it": check the hub, then void it.

Error codes: 400 `invalid_approval` (malformed, wrong network, unknown key, bad signature, too long-lived, an amount
out of range or above `max_msat`, or a void for another refund id), `approval_expired`, `invalid_invoice` (malformed,
another network's, no amount or an amount other than the approved one, or already used by another refund: one refund
per invoice), `own_invoice` (one of SatBooth's own invoices), `invalid_json` and `body_too_large`; 404
`refunds_disabled` (no
`payout` block, on every refund route), `unknown_order` and `not_found`; 409 `refund_exists` (the refund id was already
claimed), `exceeds_refundable` (the order's paid msat minus its refunds that have not failed is
below the amount), `refund_in_flight` (a void too early, while SatBooth is still sending the refund, or while the
hub reads the payment pending) and
`invalid_state` (a void of a refund that already has a result or a result waiting to be written, of one whose ledger
already shows it resolved, or of one that changed while the void asked the hub); 429 `daily_cap` and `too_many_open`
(64 open refunds); 503 `hub_unavailable` (a void whose lookup failed, or whose answer did not match the refund:
another invoice, amount or type, or a malformed answer, which also raises an `ALERT`); 401 `unauthorized`; 403
`forbidden`; 500 `internal`.

## `satbooth-approve`

The signing tool for the admin system. The key file holds the 32-byte Ed25519 seed as 64 lowercase hex digits, mode
0600 or stricter; no flag takes key material, and no error prints the key.

    satbooth-approve keygen -out <path>
    satbooth-approve refund -key <path> -network <name> -order-id <32 hex> -amount-msat <n> -expires-in <duration>
    satbooth-approve void -key <path> -network <name> -refund-id <32 hex> -expires-in <duration>

- `keygen` writes a new key file (it refuses an existing path) and prints the public key for `payout.approval_keys`;
- `refund` prints `refund_id=<32 hex>` (a fresh random id) and `approval=<296 hex>`;
- `void` prints `approval=<240 hex>`, a void approval for that refund id, which ends any open refund (see "Voiding a
  refund" above).

`-amount-msat` is plain decimal (no sign, base prefix or leading zero), a multiple of 1000. `-expires-in` is 1m to
719h: SatBooth refuses an approval valid for more than 30 days (720h) by its own clock, and the hour under it covers
a server clock up to an hour behind the admin system's.
Exit code 0 is success, 1 a failure, 2 a usage error.
