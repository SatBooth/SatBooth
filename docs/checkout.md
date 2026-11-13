# The checkout page (public listener)

`GET /c/<token>` shows the amounts (sats for Lightning, BTC on-chain, XMR for Monero, joined by "or") and the state.
For a priced order the fiat price comes first, above the amounts, as the merchant sent it with its currency
(`10.00 USD`), and when Yadio was among the order's quote sources the page ends with "Rates: yadio.io", yadio.io
linking to `https://yadio.io` (its one link out). `POST /c/<token>/pay` makes the order's address and invoice, or its
Monero subaddress (or reuses them), and redirects back to the page; `GET /c/<token>/qr.svg` is the payment text as an
SVG QR code, and `GET /c/<token>/qr.svg?rail=monero` the Monero one. The page has no JavaScript: while a payment can be made or is being checked it refreshes
itself every 5 seconds with `<meta http-equiv="refresh">`, under the content security policy
`default-src 'none'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`.
Every other path, and any token that is not exactly 32 lowercase hex characters, gets the same 404. Put the public
listener behind a CDN or WAF; the challenge before "pay" is the CDN's (threat model §1).

There is one open invoice per order: "pay" reuses it, and a new one is made only after it expires and only while the
order is open. Its description is `SatBooth order <id>`, the order's opaque id.

On-chain, the first press asks bitcoind for the order's one address, and later presses show the same one. The address
and the invoice are made at the same time; a press waits at most 13 seconds, and an address that takes longer than
10 seconds sends the customer back with "Busy, try again" (the address is still made, and the next load shows it).
No address is made for an order past its expiry or already paid on any rail. What the page shows:
- an order with both bitcoin rails: one QR of the BIP321 URI `bitcoin:<ADDRESS>?amount=<BTC>&lightning=<INVOICE>` (address and
  invoice in upper case), the address and the invoice as copy fields, "Send exactly <BTC> BTC", and an "Open in
  wallet" link with the same URI;
- on-chain only: the same without the `lightning` part; Lightning only: a QR of `LIGHTNING:<INVOICE>` and a
  `lightning:<INVOICE>` link;
- once an on-chain payment is seen: "Payment seen", "Seen, N of M confirmations", and below the amount the sats seen
  so far. Lightning is no longer offered, and the QR and the link ask only for the amount still missing;
- past the expiry: no address is offered any more (a payment then would be late);
- a URI longer than the QR limit (1,200 bytes) gets no QR; the copy fields and the link stay. An invoice that cannot
  go into a URI (it must be letters and digits starting `ln`) leaves the address-only URI, and logs one `ALERT` per
  invoice;
- "Checking payment…", beside the address, when the rail's view is stale (`onchain.stale` or `monero.stale` in the
  API: before the watcher's first good pass, when it has had no good pass for 3 poll intervals plus the last good
  pass's duration, or when the last pass could not bring this order up to date), but only for a rail the customer used:
  one with an address issued or money seen. A stale rail the customer never pressed says nothing. The page also shows
  it when the `onchain` or `monero` block is missing for an order with that rail (that page shows neither the amounts
  nor, for a priced order, the price);
- `paid`, with "The merchant will refund the extra" when more than the amount arrived (overpaid, a late payment, or
  on-chain money beside a Lightning payment); `underpaid` and `late` with "The merchant will contact you" (for
  `underpaid` after "Payment received is below the amount.", "Your payment is not yet confirmed enough." when the
  whole amount was seen on time but is not confirmed at the window's end, or "Your payment did not confirm." when the
  on-time payment died after `order.underpaid`); `expired` with "Nothing was charged" only when no rail the customer
  used is stale (else it reads "Checking payment…").

**Monero and the rail picker.** An order with a Monero amount and a bitcoin amount offers both, each with its own
form and button: "Pay with bitcoin" (`rail=bitcoin`) and "Pay with Monero" (`rail=monero`). A Monero-only order offers
"Pay with Monero" alone. Without the field a press means bitcoin when the order has a bitcoin amount, else Monero; a
field that is not exactly once `bitcoin` or `monero`, or names a rail the order lacks, gets a 400. A Monero press asks
only the Monero rail, never Lightning or bitcoind: the first press makes the order's one subaddress (a new index of
`account_index`, recorded before it is shown), and later presses show the same one. A press waits at most 10 seconds
for it, then reads "Busy, try again"; so does a press while wallet-rpc is unavailable, the startup check has not passed
or the wallet is behind on subaddresses. No subaddress is made for an order past its expiry or already paid on any
rail. What the page shows:
- the subaddress as a copy field, "Send exactly <XMR> XMR.", a QR of `monero:<SUBADDRESS>?tx_amount=<XMR>` and an
  "Open in wallet" link with the same URI. The URI carries the amount only, no `recipient_name` or `tx_description`;
  the amount is the exact decimal of the piconero, up to 12 places without trailing zeros (`0.5`, not `0.500000000000`),
  which a wallet that reads it as a double still recovers exactly, since every amount is below 2^53 piconero;
- while nothing is seen, the other rail's button stays, so the customer can still switch;
- once Monero money is seen: "Payment seen", "Seen, N of M confirmations" and, below the amount, the XMR seen so far
  ("0.3 XMR seen so far."); the bitcoin rails are no longer offered, and the QR and the link ask only for what is
  still missing;
- once bitcoin money is seen (an on-chain payment, or Lightning paid), the Monero subaddress is no longer offered;
- `paid`, with the refund line also for Monero money beside a payment on another rail, an overpaid or a late Monero
  payment.

The phone-wallet scan of the Monero QR was not done in qualification: it is a manual step (`qualify/REPORT.md`).

Both listeners limit headers to 8 KiB (a request over that gets 431), bodies to 16 KiB, and requests in flight to
256 (the rest get 503); `ReadHeaderTimeout` is 5 s, `ReadTimeout` 10 s, `WriteTimeout` 15 s, `IdleTimeout` 60 s.
