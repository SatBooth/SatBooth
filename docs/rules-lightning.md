# Rules for Lightning payments

- **Paid** means the ledger holds one transfer for the order, whose id is derived from the invoice's payment hash. A
  payment seen twice (by a notification and a poll, or again after a restart) is the same transfer.
- **Finding payments.** The service looks up every open invoice at the wallet in turn, 5 lookups a second in total.
  Notifications only move an invoice to the front of the queue; the lookup is what is believed.
- **Expiry.** An order reads `new` after its own expiry until every invoice it had is closed, because a payment may
  still arrive. An invoice is closed only on evidence that it is unpaid:
  - a successful lookup that shows the invoice not settled, made at least 2 minutes after the invoice's expiry;
  - among error answers, only the wallet's `NOT_FOUND`, and only after the deadline (24 hours after the later of
    the invoice's expiry and the service's start).

  Nothing else is evidence: not another error answer (`INTERNAL`, `RESTRICTED`, `EXPIRED`), a malformed reply, a row
  for another invoice, a settlement below the amount, or any transport failure. Past the deadline such an invoice
  stays open and is stale: its order keeps reading `new`, it stops counting against `max_open` (also after a
  restart), it is looked up every 10 minutes, and it logs a line starting `ALERT`. After a restart it is looked up
  at the normal rate again until 24 hours after the start, when it becomes stale again.

  A closed invoice is still watched. Lightning nodes accept a payment for a while after an invoice's expiry. LDK,
  the node of the hubs SatBooth is qualified against, adds 7,200 s to the expiry and compares with the latest
  block-header time it has seen, so its window is about 2 hours past the expiry, longer or shorter as block times
  lag the clock (`docs/design.md` has the source). So an invoice closed by a lookup is looked up every 10 minutes
  until its deadline. A settlement then is recorded like any other, logs a line starting `ALERT`, and the order
  reads `paid`: `order.paid` follows `order.expired`. The deadline is 24 hours after the later of the expiry and the
  service's start, so a restart inside that time extends the watch to 24 hours after the start: up to about 48
  hours after the expiry. A settlement after the deadline is not recorded, nor is one made while SatBooth is down if
  it stays down past the invoice's expiry plus 24 hours (the money is at the hub; check it there). The watch costs
  about 144 lookups per watched invoice per day, from the same lookup rate (fewer each past about 3,000 watched
  invoices at the default rate, when the rounds are full); a watched invoice between its 10 minute lookups costs
  nothing.
- **Pressing "pay"** records the attempt, then asks the wallet for the invoice on a flight that survives the customer
  closing the page. If the wallet's answer is lost, the outcome is unknown: the customer can press again, and an
  extra invoice at the hub is harmless. Before making a second invoice for an order, the service looks up that
  order's expired invoices that are not yet closed (15 seconds per lookup). If one settled, it is recorded and the
  order reads `paid`; if a lookup fails, "pay" is refused. A press the limits refuse reads "Busy, try again.": one
  more reason is the per-account cap, an `account_id` that already holds `max_open_per_account` open invoices (its
  other orders' invoices, for an account of either class). An invoice stops holding its slot when it is paid (once its payment is
  completed: for an order with an on-chain amount, after the on-chain rail has been told; if that fails, the slot is
  held until the next lookup completes it), closed, or stale, as for `max_open`.
- **The hub's invoices.** Every invoice that `make_invoice` returns is checked before it is stored or shown: the
  payment hash has the shape of one, the amount the hub reports is the order's, and the invoice's own text, exactly as
  the hub returned it (an uppercase invoice is refused), is a well-formed BOLT 11 invoice for `nwc.network` (its
  prefix and checksum) that carries an amount equal to the order's. SatBooth decodes no other field: the destination
  and the description are the hub's. An invoice that fails is not stored and not shown, and the slot is given back. The
  customer sees "Busy, try again." (the same answer as for any bad answer of the hub) and the log holds one line, with no
  part of the invoice: `ALERT: make_invoice answered with an invoice for another network or amount`. On that line,
  check which network the hub is on, and that `nwc.network` names it: a hub on another network than the configuration
  would hand out invoices that worthless coins pay. SatBooth also checks the hub's network at every start, so a
  hub on another network than `nwc.network` does not start SatBooth; this line is the second check, for one invoice:
  no order is left with an invoice and nothing is stored.
- **Alerts.** Lines starting `ALERT` mean the operator should look. Each is a fixed text. The lines about a stale
  invoice, a settlement below the amount, a second paid invoice, a late payment and an order's amount outside the limits
  carry the order's id; the lines about a bad lookup answer, an invoice closed at its deadline, a payment record with
  other details, a bad `make_invoice` answer (both lines) and a failed write of a new invoice carry none. With the
  `alerts` block, the operator's alert webhook receives them too (see [Alerts](alerts.md)). They are:
  - a stale invoice: past its deadline, and the wallet's answers do not prove it unpaid (logged at each 10 minute
    lookup while it lasts);
  - a settlement below the invoice amount (not recorded);
  - an invoice closed at its deadline on the wallet's `NOT_FOUND`;
  - a second invoice of an already paid order that was paid too (both payments are in the ledger);
  - a payment record in the ledger with other details than the lookup shows;
  - a bad answer from the wallet: a lookup for another payment hash or a bad row, or `make_invoice` answering with a
    bad invoice;
  - `ALERT: make_invoice answered with an invoice for another network or amount` (see "The hub's invoices" above);
  - a failed write of a new invoice to the store;
  - an order whose amount the current limits refuse (made under other limits; the customer sees "busy");
  - a late payment: an invoice closed as unpaid after its expiry was paid, and the order reads `paid` after
    `order.expired`;
  - a webhook event given up after 24 hours;
  - a webhook event held back because its order's stored quote is malformed (once per event per run; see
    "Webhooks");
  - `ALERT: an order reverted by a reorg or a double spend was paid again on Lightning`, logged by the on-chain or
    Monero watcher when the order's Lightning payment, not the chain's, makes an order that got `order.reverted` read
    paid again (`order.paid_again`).

  The on-chain and Monero lines are listed under their rules below.

What each end-to-end phase proves against real hubs, including a service whose relay connection is cut and restored, is
in the "Lightning end to end" section of [`qualify/REPORT.md`](../qualify/REPORT.md).
