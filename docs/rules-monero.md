# Rules for Monero payments

The on-chain rules above carry over to Monero unchanged, with these differences: amounts are in piconero, a payment is
one transaction's money to one subaddress, Monero has no RBF (so no fee bump inherits a first-seen time), and a sender
may lock a payment (`unlock_time`). Monero facts are marked *source* (Monero v0.18.5.4) or *observed* (regtest,
`qualify/REPORT.md` "Monero end to end").

- **Paid** means the ledger (ledger 3, in piconero) holds transfers for the order's payments that reach
  `monero_piconero`. wallet-rpc sums the outputs of one transaction to one subaddress into one entry, so a payment is
  a (`txid`, subaddress) pair, and one transaction that pays two orders is two payments (*observed*: one transaction
  to two subaddresses is listed with both). Each gets one transfer when it has the confirmations of the order's tier
  (1, 3 or 10 by default, picked by `monero_piconero`), with an id derived from the pair, so a payment seen twice, or
  again after a restart, is the same transfer. Confirmations are counted as Monero wallets count them: the wallet's
  height less the payment's block height, so a payment in the wallet's last block has 1. With a tier of 0, a payment
  that counts is recorded as soon as a pass finds it in the tx pool.
- **Finding payments.** Each pass asks wallet-rpc for its height, then `get_transfers` (`in` and `pool`) of
  `account_index`, from the last scan position less the settled depth (so a payment is listed again until it is
  settled), and stores the entries to subaddresses SatBooth issued. Entries for other addresses (the primary address,
  a subaddress SatBooth did not issue) are not SatBooth's and are skipped without a line; a payment listed both mined
  and in the pool counts once, mined. Then it refreshes every tracked payment with `get_transfer_by_txid`, paced at
  `poll_rate`. First seen is SatBooth's clock when a pass first stores the payment, never wallet-rpc's `timestamp`.
- **The lag.** wallet-rpc refreshes its wallet from monerod every 20 seconds (*source*: `DEFAULT_AUTO_REFRESH_PERIOD`,
  `src/wallet/wallet_rpc_server.cpp:64`), while `get_transfers` and `get_transfer_by_txid` read the tx pool on every
  call. So a payment in the pool is seen within one poll (*observed*: about 1.1 s at `poll_seconds` 2), and a block up to
  about 20 s plus `poll_seconds` after it is mined (*observed*: 6.5 s to 20.6 s from the tier's block to `paid`). A
  payment sent seconds before the expiry can therefore read `late`.
- **0-conf risk.** With a tier of 0 a payment counts from the tx pool. Monero has no RBF, but a conflicting spend can
  still win the race to a block (*inferred*): a tier of 0 trusts the payer for that amount. A record made from the pool
  is reversed when its payment is gone (wallet-rpc no longer knows the transaction) or wallet-rpc marks it
  `double_spend_seen`: `order.reverted` when the order stops reading paid, then `order.paid_again` if it is mined
  after all (*observed*: a flushed pool payment gave `order.reverted`, and its relay and a block `order.paid_again`).
  On a tier that needs a block, `double_spend_seen` reverses nothing: the payment stays a pool payment, with `ALERT: a
  Monero payment's inputs were seen double spent; the block decides` once per payment per process (at most 16 a pass,
  then one line with the count of the rest).
  `double_spend_seen` was not reproduced on regtest (it needs a conflicting transaction built outside wallet-rpc); the
  tests' in-memory wallet-rpc covers it.
- **Counted or late.** As on-chain: a payment counts when SatBooth first sees it (in the pool or in a block) before the
  order's expiry, payments that count add up (partial payments, top-ups before the expiry, overpaid with its `ALERT`),
  and a payment first seen at or after the expiry is `late`, never counts, and waits for a person. A late payment is
  recorded when it has its tier's confirmations (at least 1), under its own code; `order.late` and its `ALERT` wait for
  that record. A payment sent in time but first seen after an outage of wallet-rpc, monerod or SatBooth, or while Monero
  watching was paused, that spans the order's expiry reads `late`. A payment the wallet only finds after a rescan
  deeper than the listing window (the last scan position less the settled depth) is not listed, and so not found:
  reconcile it by hand from the merchant's wallet.
- **A sender's `unlock_time`.** An output is spendable 10 blocks after its block (*source*:
  `CRYPTONOTE_DEFAULT_TX_SPENDABLE_AGE`, `src/cryptonote_config.h:49`), about 20 minutes. A payment whose `unlock_time`
  is above 0 and above the wallet's height when SatBooth first stored it plus 10 is **never counted**, whatever its
  first-seen time: it is late. A value of 500,000,000 or more is a timestamp, not a height (*source*:
  `CRYPTONOTE_MAX_BLOCK_NUMBER`, `src/cryptonote_config.h:40`, applied in `is_tx_spendtime_unlocked`,
  `src/wallet/wallet2.cpp:7451`), and always exceeds the bound. It logs `ALERT: a Monero payment is locked by its sender
  beyond the normal lock; it is treated as late` once, when first stored, and is recorded as late once mined, with
  `order.late`. It is judged against the height stored at first sight, so it never changes as the chain grows. At
  v0.18.5.4 such a payment cannot be sent through wallet-rpc (`transfer` refuses a non-zero `unlock_time` with -50,
  *source*: `wallet_rpc_server.cpp:1266-1271`; *observed*), and monerod's pool refuses to relay one (*source*:
  `src/cryptonote_core/tx_pool.cpp:185-191`); only a miner can put one in a block. The rule stays as a defence; it is
  tested with the in-memory wallet-rpc, not on regtest.
- **Short at the expiry**, **the final event** and **the watch window**: as on-chain (`order.underpaid` or
  `order.expired` once, after Lightning is done; the 7-day window; `order.underpaid` at the window's end for an on-time
  payment still not confirmed enough, which stays tracked and can still pay the order: *observed*). On an order with an
  on-chain amount too, each rail waits for the other: no final event while the other rail is still being paid or its
  view is stale (see [The merchant API](merchant-api.md)). Past the window, an on-time payment that is gone from the wallet's listing,
  on an order with its final event and nothing standing in the ledger, stops being polled with `ALERT: an on-time
  Monero payment past its watch window left the wallet; SatBooth stops polling it and will notice it again only if a
  scan lists it mined` (not when the order reads paid by its events). A payment to the subaddress after the window is
  stored, logs `ALERT: a Monero payment arrived after the subaddress's watch window` and reads `late`, but is never
  recorded.
- **Reorgs.** A recorded payment that is no longer mined gets a reversing transfer and `ALERT: a Monero payment was
  reverted by a reorg or a double spend`; if the order stops reading paid, `order.reverted` goes out, and when the
  payment is mined again `order.paid_again` (with `ALERT: a reverted Monero payment confirmed again`). wallet2 itself
  drops the payments above a fork and refuses a reorg deeper than 100 blocks by default (*source*:
  `src/wallet/wallet2.cpp:4486-4491`, `:3454-3457`; `ORPHANED_BLOCKS_MAX_COUNT`, `src/cryptonote_config.h:77`), so
  SatBooth's store is the only record of what disappeared. *Observed*: a block popped with no new block is invisible to
  both wallets (they keep the payment mined until a new block arrives); the transaction re-mined in the next block sent
  no event; a popped payment dropped from the pool gave `order.reverted`. While the wallet's height is below SatBooth's
  scan position (a restored wallet still rescanning, or wallet-rpc moved to a monerod on a shorter fork), a transaction
  the wallet does not know yet is not taken as gone, and the orders it touches read `stale`. Confirmations, and the
  settled depth below, are always counted from the wallet's own height, which SatBooth stores beside its scan
  position at every pass, so a payment waits while the wallet catches up and is never counted with confirmations its
  wallet does not show.
- **Not detected.** A reorg of the settled depth (the last tier's confirmations, 10 by default) or more after an order
  was paid: once a payment is recorded that deep, SatBooth stores it as settled and never polls it again, also after a
  restart. Also a payment the wallet first lists below the listing window (above). Also a payment in an entry SatBooth
  could not read: the scan position still advances (holding it would let one bad entry grow every listing without
  bound), so the next listings may start above it; `ALERT: wallet-rpc listed entries SatBooth could not read; reconcile
  the wallet's transfers from from_height to to_height, and its pool, by hand` names the listing's heights (from its
  `min_height` to the scan position) to reconcile from the merchant's wallet. At v0.18.5.4 wallet-rpc always sends every
  field SatBooth reads (*source*: `transfer_entry`, `src/wallet/wallet_rpc_server_commands_defs.h:1469-1488`), so it
  takes a changed or corrupted answer. And a single payment above 2^53 − 1 piconero (about
  9,007 XMR) to an issued subaddress, above the bound of every amount SatBooth stores: it is never recorded, and `ALERT:
  a Monero payment above 2^53-1 piconero to an issued subaddress is not recorded; reconcile it by hand` names its order,
  once per payment per process; past 16 such lines in one pass, one `ALERT: more Monero payments above 2^53-1 piconero
  to issued subaddresses are not recorded; reconcile them by hand` gives the count of the rest.
- **Both rails.** Monero beside Lightning or on-chain follows rule 6.1-W: both payments stay in the ledger, the order
  reads `paid` once and sends one `order.paid`, and `ALERT: an order was paid on both rails` is logged on a record when
  another rail reads paid or holds money (counted or late). Each watched rail logs it once per order per process, so
  money on the on-chain and the Monero rail gives two lines (*observed*). The checkout page shows the refund line, and
  once money is seen on one family (bitcoin or Monero) it stops offering the other. The two watched rails settle under
  one shared lock, so neither reads the other's paid state while the other's settle changes it: a reversal on one rail
  while the other also holds money gives exactly the events the money says, never an `order.paid_again` that no
  payment backs or two reversals with no `order.reverted`. The API and the checkout page read under the same lock, so
  they never show a settle half done (a new payment recorded before an old one's reversal).
- **The watcher's cost.** Every `poll_seconds` a pass makes one `get_address` (the wallet's identity), one `get_height`
  and one `get_transfers`, then one `get_transfer_by_txid` per tracked payment, paced at `poll_rate`, and at its end
  `get_height` and `get_address` again, so a pass with N tracked payments takes about N / `poll_rate` seconds. A
  payment is tracked until it is recorded the settled depth deep, or, if it never will be, until its order's window
  closes (and an on-time one past it, as above). One `ALERT: wallet-rpc call failed` (with the method and the error
  code) ends a pass whose `get_height`, `get_address` or `get_transfers` fails; until a pass succeeds nothing is
  recorded and no event is added, and the next good pass catches up from the stored scan position.
- **A pass writes only what it read from an unchanged wallet.** A pass stores nothing until its last call. If the
  wallet's height went down meanwhile (a reorg onto a shorter chain, a daemon switch, a rescan), the wallet is another
  one, or a press saw another wallet, nothing of the pass is stored and the next pass reads the wallet again (a line
  `the wallet changed while the pass read it; …` with the reason; an `ALERT` at the third in a row). Confirmations
  are never above the count wallet-rpc gave with the payment's own answer. Not detected: a wallet switched to another
  and back within one pass with no press in between, or with a press that saw the other wallet just as the pass
  began to write (it pauses watching after that pass; see `docs/threat-model.md`).
- **Alerts.** Lines starting `ALERT` mean the operator should look; each is a fixed text, with an order id, a count, a
  one-word reason or an RPC method and error code, never an address, a key or wallet-rpc's text:
  - `ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record` (with the count);
  - `ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it catches up` (at startup,
    and once per pass while it lasts);
  - `ALERT: a new subaddress failed its checks; not shown` (with the step number);
  - `ALERT: the wallet-rpc check failed; subaddresses are not issued` (a re-check failed while the wallet is behind on
    subaddresses);
  - `ALERT: Monero watching is paused: the wallet failed its startup check` (with the `reason`; once per pass while it
    lasts, `reason=address` also when a pass or a press finds the wallet's base address changed);
  - `ALERT: wallet-rpc call failed` (one per failed pass);
  - `ALERT: wallet-rpc answered with a bad transfer` (once per payment per process, at most 16 a pass);
  - `ALERT: wallet-rpc answered with more bad transfers` (with the count of those past the 16 in one pass);
  - `ALERT: the wallet changed during several passes in a row; nothing was written from them: is wallet-rpc's wallet
    or monerod changing?` (at the third discarded pass in a row, once until a pass is stored);
  - `ALERT: wallet-rpc listed entries SatBooth could not read; reconcile the wallet's transfers from from_height to
    to_height, and its pool, by hand` (with the count, `from_height` and `to_height`; once per pass while it lasts:
    entries the client could not decode, or of the wrong type, account or amount, with a malformed address, or a key
    repeated in one list; a coinbase to the account, type `block`, is skipped without it; see "Not detected");
  - `ALERT: a Monero payment above 2^53-1 piconero to an issued subaddress is not recorded; reconcile it by hand` (with
    the order id; once per payment per process, at most 16 a pass);
  - `ALERT: more Monero payments above 2^53-1 piconero to issued subaddresses are not recorded; reconcile them by hand`
    (with the count of the oversized payments past those 16 in one pass; at most once a pass);
  - `ALERT: the wallet's height has not moved for 30 minutes; is monerod running and synced?` (then at most once every
    30 minutes while it lasts: a wallet that lost its daemon keeps answering with the same height);
  - `ALERT: unused Monero subaddresses exceed unused_alert; raise the merchant wallet's subaddress lookahead` (with the
    count; at the crossing, then once a day);
  - `ALERT: a Monero payment arrived after the subaddress's watch window`;
  - `ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late`;
  - `ALERT: a Monero payment's inputs were seen double spent; the block decides` (once per payment per process, at
    most 16 a pass), and `ALERT: more Monero payments' inputs were seen double spent; the block decides` (with the
    count of the rest);
  - `ALERT: a late Monero payment arrived after the order's expiry` (a locked payment is late too);
  - `ALERT: a Monero payment was reverted by a reorg or a double spend`;
  - `ALERT: a reverted Monero payment confirmed again` (or the on-chain or Lightning line, by the rail whose money makes
    the order paid again);
  - `ALERT: an on-time Monero payment past its watch window left the wallet; SatBooth stops polling it and will notice
    it again only if a scan lists it mined`;
  - `ALERT: a Monero order left its watch window without a final event; check it by hand` (once per order, and again
    after a restart within a week of the window's end);
  - `ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet`
    (with the order id, also when the order has its final event; once per payment per process, at most 16 a pass, and
    again after a restart within a week of the window's end): a late payment whose `get_transfer_by_txid` failed in
    every pass of its window has no mark, is no longer polled and is not recorded; `ALERT: more Monero payments left
    their watch window without one successful observation; reconcile them from the wallet` (with the count of the rest);
  - the rail-generic lines, with the on-chain texts: `ALERT: an order was overpaid; refund the excess`, `ALERT: an
    order was underpaid at expiry`, `ALERT: an order was underpaid at the end of its watch window; its payment is not
    yet confirmed enough and stays tracked`, `ALERT: an order was paid after its order.expired or order.underpaid
    event; the payment is recorded and the order reads paid`, `ALERT: an order was paid on both rails` and `ALERT: a
    payment record exists with different details`.

  Other lines without the prefix: `wallet-rpc version` (info, with the major and the minor, once per process),
  `wallet-rpc call failed for a payment` (one payment skipped for a pass), `the wallet changed while the pass read it;
  nothing from the pass is written, the next pass reads the wallet again` (a warning, with `reason` `height`, `address`
  or `generation`), `scan failed`, `track failed`, `settle failed` and `Monero status could not be read`. The `at` of
  `scan failed`, `track failed` and `settle failed` names the read or write that failed (one fixed word, never from the
  error or the data); a stop of the service is not logged as a failure.

What each Monero phase proves against a real monerod and a view-only wallet-rpc, including a reorg, a pool drop, a
restart and a restored wallet file, is in the "Monero end to end" section of [`qualify/REPORT.md`](../qualify/REPORT.md).
