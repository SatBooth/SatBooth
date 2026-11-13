# Rules for on-chain payments

- **Paid** means the ledger holds transfers for the order's payments that reach `onchain_sat`. Each payment output
  (`txid`, `vout`) to the order's address gets one transfer when it has the confirmations of the order's tier (1, 3
  or 6 by default, picked by `onchain_sat`), with an id derived from the output, so an output seen twice, or again
  after a restart, is the same transfer. With a tier of 0 confirmations, a payment that counts is recorded as soon as
  a pass finds it in bitcoind's mempool.
- **0-conf risk.** With a tier of 0 confirmations a payment counts from the mempool. Since Bitcoin Core 28, full RBF is
  the default (*source*: the Core 28.0 release notes, "The default value of the `-mempoolfullrbf` configuration option
  has been changed from 0 to 1"), so the payer can replace an unconfirmed payment with one that pays someone else until
  it is mined. SatBooth then reverses the record and sends `order.reverted` (when the order, all rails together, stops
  reading paid; *observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", for a record from the mempool and its reversal
  when `bumpfee` diverted the payment), but whatever was fulfilled in between is the merchant's loss: a tier of 0 means
  trusting the payer for that amount. A record made from the mempool is reversed only when its payment is gone
  (replaced, double spent, or evicted); a reorg that puts it back in the mempool keeps it. Inside the watch window, an
  evicted record that comes back is recorded again: `order.reverted` when it left (if the order stopped reading paid),
  then `order.paid_again` when it is back (once the order has its final event, a payment that is gone and has no record
  is read with a backoff, so a return to the mempool with no block is seen up to 10 minutes later and the record waits
  that long; see "The watcher's cost"). It is settled only once mined `SettledDepth` deep. A late payment is never
  recorded from the mempool, whatever the tier: it needs at least 1 confirmation, so a late payment replaced before a
  block sends no `order.late`. A payment that counted, was evicted before the expiry and was broadcast again after it
  keeps its first-seen time, so it still counts and is recorded from the mempool once it is back (tested with the test
  bitcoind, not observed on regtest). A fee bump that keeps the output can land between a pass's scan and its
  `getmempoolentry` of the original: the original then dies before its replacement can be listed, so the pass that marks
  it dead does not reverse its record when that would turn the order unpaid. The next pass records the bump and reverses
  the original as it would in one pass, with no `order.reverted` and no `order.paid_again` (tested with the test
  bitcoind; *observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", `TestOnchainZeroConf` B, that the split happens on
  regtest and that the bump now sends neither event). The cost: a payment really gone (replaced away, double spent or
  evicted) is reversed, and sends `order.reverted`, one pass later than it is seen gone; until then the order still
  reads paid. bitcoind's wallet learns of mempool changes through a queue, so it can be seconds behind its own node on
  slow storage. In that time the node has already dropped a payment that was replaced, and the wallet shows neither a
  conflict nor the replacement. SatBooth therefore asks the node, before it marks a payment that left the mempool as
  gone, whether another mempool transaction spends the payment's inputs (`gettxspendingprevout`). If one does, the pass
  marks nothing for that payment and counts it as zero if it is not recorded yet (a recorded one keeps its record), the
  order's other payments go on as in any pass, its final event waits (on a mixed order the Monero rail's too: it reads
  the order's on-chain side as not yet judged, and does not report the order as left without a final event meanwhile, as
  long as the on-chain rail is fresh), and one `WARN` line is logged; the payment is judged once the wallet has
  processed the replacement, or 60 seconds after SatBooth first found it replaced, whichever is first. One fee bump is
  therefore not reported reverted for the wallet's lag if the wallet shows the replacement within those 60 seconds
  (bitcoind's lag was measured at 16 s). After them the payment is marked gone, and when the wallet has still not listed
  the replacement one pass later: on a 0 tier the order gets `order.reverted` and, when the wallet lists the
  replacement, `order.paid_again` with its `ALERT`; on a tier above 0 across the expiry, `order.expired` and then the
  paid-after-final `ALERT`. A further bump the wallet has not processed by then reads the same. There is no longer wait
  for one output, because a payer could renew one for as long as they pay replacement fees and keep a double-spent
  payment reading paid. The 60 seconds count from the first find, also when the payment comes back into the mempool
  meanwhile or was marked gone before, and are checked when a pass reads the payment and again finds it replaced (the
  payment is marked gone at the first such pass at or after them, and reversed one pass later: up to 60 seconds plus two
  passes and the length of one pass): a pass that cannot read it (bitcoind's mempool still loading, a failed call) does
  not count, so there is no wall-clock limit while reads fail, and those states have their own log lines. Each of the
  two waits, the 60 seconds and the one pass after the mark, is granted once per paying output (one transaction output
  of the order) in a running process, and is kept in memory until the process ends: an output that left the mempool,
  came back and leaves again gets no new 60 seconds and no second hold. What is left of its first 60 seconds still
  applies, so it is marked gone and reversed in the pass that finds it gone only once those are over. A fee bump of such
  an output that lands between a pass's scan and its read is not held either: the order reads `order.reverted`, then
  `order.paid_again` with its `ALERT`. Each restart grants each wait once more. A limit of a 0-confirmation tier: every
  new paying output, such as the replacement of a fee bump, has its own two waits, so a payer who keeps putting in
  replacements that pay the order and removing them again can keep the order reading paid, without `order.reverted`,
  while no paying transaction is in the mempool most of the time. Per order the bound is 60 seconds plus two passes
  since a paying output of the order was last in the node's mempool, and it ends when one of the transactions confirms.
  A payment whose block was reorged out is marked gone at once, as before. The cost: a 0-confirmation payment that was
  really double spent is reported reverted when the wallet has seen the conflict or the 60 seconds are over, whichever
  is first (and one pass later, as above), not when the node has; until then the order still reads paid. A payment that
  nothing in the node's mempool spends (evicted, or expired) is marked gone as before. When you reconcile a 0 tier by
  hand, note that bitcoind's `getreceivedbyaddress` at `minconf` 0 counts both a replaced transaction and its
  replacement until one of them is mined, so it overstates the receipt meanwhile (*observed*: same section).
- **Counted or late.** A payment counts when SatBooth first sees it (in the mempool or in a block) before the order's
  expiry; its confirmations may come later. Several payments that count are added up: at the amount the order is
  paid, above it also overpaid (an `ALERT`; refund the excess). The customer can top up before the expiry. A payment
  first seen at or after the expiry is `late`: it never counts toward paid, the order reads `late` as soon as it is
  seen, and a person credits or refunds it on-chain. Nothing late is fulfilled automatically. A late payment is
  recorded in the ledger when it has its tier's confirmations (at least 1), under its own code, apart from paid; **`order.late`
  (once per order) and its `ALERT` wait for that record**, so a late payment that is replaced or double spent before it
  confirms sends no event (the API's `late` state and `late_sat` show it while it is live).
- **First seen is SatBooth's clock.** A payment sent in time but first seen after an outage of bitcoind or SatBooth
  that spans the order's expiry reads `late` and waits for a person.
- **Fee bumps.** CPFP and acceleration services keep the txid, so the payment keeps its first-seen time. An RBF
  replacement has a new txid; it **inherits the first-seen time of the transaction it replaced** when all of these
  hold, so a fee bump after the expiry still counts and pays the order:
  - SatBooth first lists the replacement inside the order's watch window (before 7 days after the expiry). A bump
    first listed at or after the window's end is a new, `late` payment, whatever it replaces, and a person credits it:
    otherwise a payer could keep an on-time payment unconfirmed at a low fee and bump it at the quoted amount weeks
    later, a free option on the quote;
  - bitcoind lists that transaction in the replacement's `walletconflicts`, which holds every wallet transaction
    that spends one of the same inputs (*source*: Core 32.1 `src/wallet/wallet.cpp:695-715` and
    `:2093-2100`, `src/wallet/rpc/transactions.cpp:47-50`; *observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", for a
    bump listed while both transactions were unconfirmed), and all its outputs to the order's address were first
    seen before the expiry;
  - the replacement pays the address **exactly** what that transaction paid it, in total, per transaction (all its
    outputs to the address inherit, or none: a bump that pays more, less, or another address is a new payment);
  - every other payment of the order first seen before the expiry, and every new payment the same scan already
    counted, is among the transactions it replaces (with one that it does not conflict with, the two could both
    confirm and count more than was paid on time).

  Any other replacement is a new payment with its own first-seen time, so after the expiry it is `late`,
  and so is a bump of a payment that was itself late. The rule errs toward late: a customer who paid in two
  independent parts on time and bumps one after the expiry gets that part `late`, and so does one who cancelled an
  on-time payment, paid again, and bumped the second. It is decided once, when the replacement is first stored: a
  node that heard the replacement before the original gives it its own first-seen time. An inherited time is never
  later than SatBooth's clock (after a clock step back it is the clock's time, which can be earlier than the
  original's, and even than the order's creation). The original goes `dead`. On a tier of 1 or more it was never recorded, so nothing reverses
  (*observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", for a bump after the expiry that paid the order); on a 0 tier its
  record is reversed and the replacement's made, in one pass or two, with no event either way.
- **Short at the expiry.** Past the expiry with payments that count but fall short, the order reads `underpaid`
  (`order.underpaid` once, an `ALERT`) and waits for a person. With nothing counted it reads `expired` (`order.expired`
  once). An order gets one of the two, at the first check after its expiry (or, for the whole amount seen on time but
  unconfirmed, at the window's end: see "The watch window"); later changes show only in the API (a top-up seen before
  the expiry that confirms later still makes it paid, and sends `order.paid`). Like every final event, it waits until
  the order's Lightning side is done (its invoices closed, or no Lightning amount).
- **Reorgs.** The watcher re-reads every tracked payment each pass. A recorded payment that is no longer in the chain
  gets a reversing transfer (the ledger is never edited) and an `ALERT`. If that takes the order below its amount,
  and it is not paid by Lightning, the order reads unpaid again and `order.reverted` goes out with the next `seq`. If
  the payment confirms again, it is recorded again, the order reads `paid`, and `order.paid_again` goes out with that
  `seq` (and an `ALERT`). A payment re-mined in another block keeps its record. After a reorg puts it back in the
  mempool the rail reads `seen` (`underpaid` past the expiry, where `order.reverted` backs it); on a 0 tier the record
  stays and the order stays paid (it counts from the mempool).
- **The watch window.** The address and its payments are watched until 7 days after the order's expiry. An order that a
  pass did not reach is read in a later one, and a late payment that the scan listed for it is still
  handled then, as it would have been in the pass that found it. When the
  window closes with on-time payments that reach the amount but are not confirmed enough to be recorded, the order
  reads `underpaid` and gets `order.underpaid` with `ALERT: an order was underpaid at the end of its watch window; its
  payment is not yet confirmed enough and stays tracked` (once its Lightning side is done, and unless it already has a
  final event). Those payments **stay tracked** past the window:
  - mined at the tier, one is recorded: the order reads `paid`, and `order.paid` follows `order.underpaid` with its
    `ALERT` (*observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", for the window's `order.underpaid`, then `paid` after
    one block);
  - gone (replaced, double spent or evicted), it is marked `dead`; once the order has its final event and nothing of
    the payment stands in the ledger, SatBooth stops polling it, with `ALERT: an on-time on-chain payment past its
    watch window left bitcoind's mempool; SatBooth stops polling it and will notice it again only if it is mined`
    (not when a fee bump that inherited its first-seen time carries the payment on; SatBooth matches the two by that
    time, to the second, so an unrelated payment of the order first seen in the same second can also silence it). If
    a later scan lists it mined, it is tracked again and recorded by the usual rules. A rebroadcast seen only in the
    mempool does not bring it back, so an order at a 0 tier then waits for a block. Between the order's final event and
    the window's end, a payment that comes back to the mempool with no block is seen up to 10 minutes later. The same `ALERT` comes at the
    window's end for every on-time payment that died earlier and was never carried on (a cancelled or double-spent
    on-time payment, for example), unless the order reads paid by its events (`order.paid`, with no `order.reverted`
    that `order.paid_again` has not answered): a person has nothing to do then. SatBooth stops polling it either way.
    An order whose
    payment died after its `order.underpaid` still reads `underpaid`, as its event says (never `expired`), with
    `received_sat` 0; the page says "Your payment did not confirm. The merchant will contact you.";
  - a payment whose ledger record has other details (`ALERT: a payment record exists with different details`),
    mined or dead, never settles and is polled until it is put right by hand.

  How long a stuck payment stays in bitcoind's mempool (*source*, Core 32.1; the file and line of each step are in
  `docs/design.md` §3, plan 6.1, Ruling 6.1-3): bitcoind drops an unconfirmed transaction `mempoolexpiry` hours after
  it entered its mempool (default 336); a rebroadcast through bitcoind while it is there does not reset that time,
  nor does a restart. The watch-only wallet itself rebroadcasts it: it resubmits every unconfirmed wallet
  transaction, a received one included, to the mempool and to peers every 12 to 36 hours, and to the mempool alone
  each time the wallet loads, unless `-walletbroadcast=0` (default 1; `-blocksonly` turns it off). So the payment comes back
  after `mempoolexpiry` (if its fee still gets it in), but a pass in between finds it gone and marks it `dead`, and
  past the window that ends its tracking (above). A stuck payment is therefore polled until it is mined, replaced or
  double spent, or until the first pass that finds it out of the mempool: normally at most about 336 hours after it
  last entered bitcoind's mempool (*observed*: `qualify/REPORT.md`, "Plan 6.1 follow-ups", for a stuck payment still in
  bitcoind's mempool across a restart of SatBooth, with bitcoind left running; a restart of bitcoind and the expiry
  itself are not observed).

  A late payment is not watched past the window: one not recorded by then is recorded only when its order is settled
  again (see "Confirmations are bitcoind's own count" below), and one recorded but not yet `SettledDepth` deep is not
  polled again (a later reorg of it is not detected). A payment to the address
  after the window (a fee bump of a stuck on-time payment included: past the window it inherits nothing) is stored,
  logs an `ALERT` and reads `late`, but it is never
  polled for confirmations or recorded in the ledger, so it sends no `order.late`: the `ALERT: an on-chain payment
  arrived after the address's watch window` is the signal. An order whose window closes without a final event for
  another reason (its settle or its payments' calls kept failing, its Lightning side is still not done, or its Monero
  rail is still being paid or stale) logs
  `ALERT: an on-chain order left its watch window without a final event; check it by hand`; its on-time payments
  stay tracked, and can still settle it. **Upgrading from a plan 6 build:** the first pass polls again every on-time
  payment that was never settled, whatever its window, and `satbooth` keeps refusing to start without the `onchain`
  block until they are all settled.
- **Confirmations are bitcoind's own count.** A mined payment that is not yet recorded counts at most the
  `confirmations` that `gettransaction` answered for that transaction, in the last completed pass and for the block its
  mark names, and counts zero without such a reading. One rule gives the paid decision, the permanent settle and the
  count the API, the checkout page and the status show. So
  nothing is recorded, as paid or as late, or settled for good, without a reading in the same pass. bitcoind's tip can
  go down after a pass has read it (a reorg onto fewer blocks, or a bitcoind that restarted with a shorter chain); a
  payment then waits for the confirmations bitcoind really reports. Nothing is stored for this. A late payment of an
  order past its window is read again in the pass that is about to settle its order (a new payment to its address, or an
  on-time payment still followed), when its last mark is in the mempool or mined, and is recorded late from that
  reading; it always goes to a person, never counted as paid. If the transaction is gone, it is not recorded and sends
  no `order.late`. A late payment still in the mempool when its window closes, and mined later, is recorded late
  (`order.late` and its `ALERT`) when its order is next touched and it has its tier's confirmations; a dead one is not
  read again. This costs no extra bitcoind call in the normal case, and one `gettransaction` a pass (two while it is
  unconfirmed, six in the pass that finds it gone) for such a payment in each pass in which its order is touched. If
  that read is refused or answers badly, the payment is not recorded in that pass, the order reads stale, and its
  on-time payments are still recorded; if bitcoind's mempool is still loading, it is not recorded in that pass and the
  order is not marked stale; a transport failure ends the whole pass, which records nothing. What stays: a recorded
  payment's count is not bounded (it decides nothing), and a recorded payment that is settled for good or was not read
  in the last pass shows the scan position's count; an unrecorded payment shows 0 confirmations until a
  pass has read it (after a start, or when its read failed); a late payment of a past-window order that is never touched
  again is never read and never recorded; and a late payment that arrived after the window is not recorded (its `ALERT`
  names its order).
- **Not detected.** A reorg of the largest tier's confirmations (`SettledDepth`, 6 by default) or more after an order
  was paid: once a payment is recorded that deep (and a poll has just verified it), SatBooth stores it as settled and
  never polls it again, also after a restart. Also a payment first mined between
  a scan position bitcoind can no longer list from and the tip, and never seen in the mempool (see the re-anchor
  `ALERT` below; reconcile that window by hand from the merchant's wallet).
- **Both rails.** An order paid on Lightning and on-chain keeps both payments in the ledger, reads `paid` once, sends
  one `order.paid`, and logs `ALERT: an order was paid on both rails` whichever rail pays second, also after the
  on-chain payment is settled. The rule is the same on both rails: Lightning reads paid, and the order has a recorded
  on-chain payment of any amount, counted or late, that is not reversed and is not a record with other details; a
  person refunds one rail. With a Monero amount too, the on-chain rail also logs it on a record when the Monero rail
  reads paid or holds money (counted or late), and the Monero rail does the same the other way, so money on two rails
  can log the line twice, once per rail (see [Rules for Monero payments](rules-monero.md)). The watcher checks it at every settle of the order, so a settle that failed or crashed
  before its `ALERT` is healed by the next pass, and Lightning's completion checks it when Lightning pays second, at
  any depth. It is logged once per order per process, so it can come once more after each restart. A late payment on
  a Lightning-paid order logs both its late `ALERT` and this one. The `ALERT` can come in the same pass as a reorg's
  reversal of that on-chain payment, so a person may find the payment reversed. It is lost only when the on-chain
  payment is already settled deep and the Lightning payment is completed by a path that does not tell the on-chain
  rail: the startup completion of a closed invoice whose call to the rail fails, or the reconciler's heal of a closed
  invoice after a crash between its transfer and its event. The refund line on the page and `onchain` in the API
  still show it. Once an on-chain payment is seen, the checkout page offers no Lightning.
- **The watcher's cost.** Every `poll_seconds` a pass makes one `getblockchaininfo`, one `getmempoolinfo` and one
  `listsinceblock` from the last scan position, then
  refreshes the tracked payments with one `gettransaction` for each transaction (and a `getmempoolentry` if it has no
  confirmation), shared by all its payments to SatBooth's addresses and paced at `poll_rate` transactions a second, so
  the load on bitcoind follows the number of transactions, not of outputs, and does not follow traffic. Before it
  marks a payment gone, it asks again: one more `getmempoolinfo` (bitcoind may have restarted inside the pass and not have loaded
  its mempool yet), `getmempoolentry` and `gettransaction` (a block may have mined it since the first read), and
  then one `gettxspendingprevout` (does another mempool transaction spend its inputs: bitcoind's wallet may be behind its node,
  and then the payment is not marked gone for up to 60 seconds, and is asked again in each pass, at the same
  cost); a payment already
  marked gone costs what a live one costs (two calls per transaction: `gettransaction` and `getmempoolentry`; a live
  unconfirmed one costs the same two, and the pass that marks it gone six) until its order has its final event
  (`order.paid`, `order.expired` or `order.underpaid`). From then on a payment that is gone, has no record in the
  ledger and is not foreign rests: it is read 2 × `poll_seconds` after its last read, then twice as long after each
  read, up to every 10 minutes (20, 40, 80, 160, 320 and then 600 seconds at the default); at the end of its order's
  watch window (which settles an on-time one; a late one just leaves the watched set); and at once when a scan lists it
  mined. It does not count toward the number of payments of an order that one pass reads, and a pass reads at most that
  number of an order's resting transactions: the rest wait for the next passes, so the reads at a window's end, or
  after a start, of an order with many of them are spread over several passes. A payment that comes back to the
  mempool with no block is seen at its next read, at most 10 minutes later. The backoff is kept in memory: after a
  start, each resting payment is read once, then follows it again. Accepted residual: at the 10-minute ceiling, K
  resting transactions cost about K / 60 reads in a pass of 10 seconds, so thousands of replaced transactions of orders
  that have their final event can fill a pass's reads; discovery still runs, other orders are still served in turn, and
  each replacement costs the payer a higher fee. The answers are kept for the pass only, so
  each payment is still judged on its own answers' checks and its own mark, but a transaction paying many outputs
  costs the calls of one. A payment is tracked until it is recorded `SettledDepth`
  deep; one that never will
  be (replaced, double spent, evicted, or still unconfirmed) is tracked until its order's watch window closes, and an
  on-time one past it (see "The watch window"): finding those scans the stored payments once a pass. A settled
  payment is stored as such, so after a start only the payments not yet settled are polled; a payment whose poll
  failed, or that a pass did not reach, is not settled. A pass reads the orders in turn, each one whole, for about
  `poll_seconds`: it starts no new order once `poll_seconds` have passed since it began its reads, and an order it has
  started it finishes, so one order with many payments stretches a pass by at most its own reads. The next pass starts
  with the first order the last one did not reach. An order that is not reached waits for its turn, is not settled
  meanwhile, and keeps its place; the discovery of new payments (their first-seen times) runs in every pass. Meanwhile the
  confirmation count it shows can read 0, as it does after a start, until a pass reads it. The page
  reads "Checking payment…" when no pass has succeeded for 3 poll intervals plus the last good pass's duration, so a
  long healthy pass does not trip it; for one order alone when its settle or a call for one of its payments failed in
  the last pass, or when it has gone unreached for more than 3 × `poll_seconds` (the API's `onchain.stale`). An order
  with more separate payments than one pass reads (`poll_seconds` × `poll_rate`, at least 8) is not read or settled:
  it reads "Checking payment…" and gets `ALERT: an on-chain order has more payments than one pass reads; it is not
  settled; check it by hand` (with its id, at least once per order each time SatBooth starts), and the merchant handles it by
  hand. A payment with many outputs is one read and does not count more than one. bitcoind not answering ends the pass with one `ALERT:
  bitcoind call failed` (with the method and the error code). Until a pass succeeds nothing is recorded and no event is
  added (a Lightning-only order's `expired` follows the clock and still shows in the API; an on-chain order's
  `expired` and `underpaid` wait for their events); the next good pass catches up from the stored scan position.
- **Alerts.** Lines starting `ALERT` mean the operator should look; each is a fixed text, with an order id, a count,
  or an RPC method and error code, never an address or bitcoind's text:
  - `ALERT: bitcoind is in initial block download or behind its headers` (each pass while it lasts);
  - `ALERT: bitcoind call failed` (one per failed pass);
  - `ALERT: wallet next_index is behind SatBooth's record; addresses are not issued`, and `ALERT: the bitcoind check
    failed while the index is behind; addresses are not issued` when the re-check fails for another reason;
  - `ALERT: a new address failed its checks; not shown`;
  - `ALERT: bitcoind answered with a bad transaction`;
  - `ALERT: bitcoind cannot list from the scan's cursor block; the scan is retried from the tip: if that succeeds,
    payments first mined between that block and the tip are not discovered (tracked payments still refresh)` (it
    fires on any JSON-RPC error to `listsinceblock` from the stored position, not only a lost block, and on an answer
    too large to read, over 8 MiB; never on a timeout or another transport error. When the retry from the tip fails
    too, `ALERT: bitcoind call failed` follows and the scan position stays where it was);
  - `ALERT: unused on-chain addresses exceed gap_alert; raise the merchant wallet's gap limit` (at the crossing, then once a day);
  - `ALERT: an order was overpaid; refund the excess`;
  - `ALERT: an order was underpaid at expiry`;
  - `ALERT: an order was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays
    tracked`;
  - `ALERT: a late on-chain payment arrived after the order's expiry`;
  - `ALERT: an on-chain payment arrived after the address's watch window`;
  - `ALERT: an on-time on-chain payment past its watch window left bitcoind's mempool; SatBooth stops polling it and
    will notice it again only if it is mined` (at or after the window's end, for each on-time payment that is gone and
    was not carried on by a fee bump, whether it died then or earlier, unless its order reads paid);
  - `ALERT: an on-chain order has more payments than one pass reads; it is not settled; check it by hand` (at least once per
    order each time SatBooth starts; see "The watcher's cost");
  - `ALERT: an on-chain order left its watch window without a final event; check it by hand` (an order whose settle,
    or whose payments' calls, kept failing, whose Lightning side is still not done, or whose Monero rail was still
    being paid or stale at the window's end; no event is added. Once per
    order, and again after a restart within a week of the window's end. When the pass at the window's end holds the
    final event because an on-time payment died after its scan, the check waits one pass: the next pass reports the
    order only if it still has no final event. The same holds for an order that a pass did not reach: it is held
    until its turn, and the Monero rail of a mixed order holds its own final event meanwhile. An order too wide for one pass is never settled, so it gets this
    `ALERT` too);
  - `ALERT: an on-chain payment was reverted by a reorg`;
  - `ALERT: a reverted on-chain payment confirmed again`, or `ALERT: an order reverted by a reorg or a double spend
    was paid again on Lightning` when Lightning's payment, not the chain's, makes the reverted order paid again (the
    Monero rail's own line when its payment does);
  - `ALERT: an order was paid after its order.expired or order.underpaid event; the payment is recorded and the order
    reads paid`;
  - `ALERT: an order was paid on both rails`;
  - `ALERT: a payment record exists with different details`;
  - `ALERT: an open invoice is not fit for a payment URI; the QR and the link do not carry it` (the checkout page, once
    per invoice per process).

  Other error lines without the prefix: `bitcoind call failed for an output` (one payment skipped for a pass),
  `bitcoind listed entries SatBooth could not read` (with the count), `bitcoind is still loading its mempool;
  unconfirmed payments wait for the next pass` (once a pass, after a bitcoind restart, until it has loaded its
  mempool: no payment is marked gone meanwhile), `bitcoind's wallet has not yet processed a transaction in the node's
  mempool that replaces a payment; the order is judged when it has` (once a pass, while bitcoind's wallet is behind its
  node after a fee bump or a double spend: that payment is left unjudged and nothing is marked), `scan failed`, `track
  failed`,
  `settle failed` and `on-chain status
  could not be read`. The `at` of `scan failed`, `track failed` and `settle failed` names the read or write that failed (one fixed
  word, never from the error or the data); a stop of the service is not logged as a failure.

What each on-chain phase proves against a real bitcoind, including RBF, reorgs and the index after `SIGKILL`, is in the
"On-chain end to end" section of [`qualify/REPORT.md`](../qualify/REPORT.md).
