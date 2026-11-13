# SatBooth threat model

Assume that creating accounts, identities (npubs) and IP addresses is nearly free for an attacker.
**Per-account limits are worth almost nothing against Sybils.** Every limit that protects a node or the
money must be global, and the design must keep paying customers working while an attacker hits the global
limits.

## 0. Full compromise of the SatBooth server

This is the design goal. On-chain bitcoin and Monero are watch-only/view-only, so the attacker can spend
neither. Lightning losses are bounded by:
- the hub's channel balance (kept small, swept often);
- the `payout` connection's hub-enforced lifetime budget (renewal `never`), raised by a person in the hub when it runs low. A leaked payout key loses at most
  what is left of it.

The attacker also gets privacy data: addresses, payment history, order metadata. Keep that minimal (see 8).

Since plan 10 an alert payload carries no secret: it holds the fixed message of an `ALERT:` line and only attributes
that pass an allowlist of keys and value shapes (opaque ids, non-negative integers, one-word reasons), so no key,
address, invoice, amount or peer text reaches the operator's webhook (§9). The alert secret is a secret file like the
webhook secrets. An attacker who holds the server holds that secret too and can forge or withhold alerts, so alerts do
not detect a compromise of the server itself.

They also get SatBooth's bitcoind RPC password. `rpcwhitelist` limits that user to twelve methods, none of which sends
money, but the whitelist is per method, not per wallet (`src/httprpc.cpp:107-118` in Core 32.1; observed,
`qualify/REPORT.md`): the user can read and draw addresses from every wallet the node has loaded. And
`listdescriptors` with `private` true returns the private descriptors of a loaded wallet that holds keys and is not
locked (source, `src/wallet/rpc/backup.cpp:479-519`; not observed). **So the bitcoind that SatBooth reaches must have
no wallet with private keys loaded**, unless it is encrypted and stays locked (`docs/bitcoind.md`, "Setting up
bitcoind"). With that rule, the on-chain bound above holds; without it, it does not. Since plan 6.1 `docs/bitcoind.md`
recommends a bitcoind dedicated to the hub and SatBooth for that reason.

Monero (plan 8, written before the code, **as built**): they also get SatBooth's
`monero-wallet-rpc` login. It is HTTP Digest on a full RPC surface: wallet-rpc has no per-method whitelist, and its
restricted mode ("Restricts to view-only commands") cannot run a checkout, because it denies `create_address`,
`get_transfers`, `get_transfer_by_txid` and `store` among others (*source*: monero-project/monero tag v0.18.5.4,
`src/wallet/wallet_rpc_server.cpp:142` for the flag; the denials at `:761-763`, `:2083-2087`, `:2906-2910` and
`:2983-2987`). Whoever holds the login can point the wallet at a daemon they run (`set_daemon`) and so fake Monero
payments (*inferred*: the wallet trusts its daemon's chain), stop or rescan the wallet, and read the view key
(`query_key` with `view_key`). On a view-only wallet nothing can be signed: `sign_transfer` refuses a watch-only wallet
with code -29 (*source*: same file, `:1382-1387`). SatBooth sends only `get_version`, `get_height`, `get_address`,
`create_address`, `store`, `get_transfers`, `get_transfer_by_txid` and `sign_transfer` (the last only with an empty set,
as the view-only probe), but that is SatBooth's own restraint, not wallet-rpc's. So the login is a SatBooth compromise,
which already writes the ledger: inside this section's bound, **provided nothing but SatBooth reaches wallet-rpc** (the
private link, the operator's firewall). SatBooth holds no Monero key: the operator creates the view-only wallet,
SatBooth's configuration carries only its expected primary address (checked at startup), and the Digest password is
loaded in-process from a 0600 file, never from argv. monerod and wallet-rpc never run on the public host: they run on a
third private host, or else on the private hub host (`design.md` §3, plan 8, decision 2). As built: `xmrrpc` refuses
any method outside the eight before sending it, and refuses a wallet-rpc that answers without asking for a login
(`--disable-rpc-login`), on every call; the startup check refuses a wallet whose primary address is not the configured
one, and one that can sign (the empty `sign_transfer` must answer -29; a full wallet answered -39 on regtest,
`qualify/REPORT.md`); `docs/monero.md` tells the operator to give wallet-rpc its login in a 0600 `--config-file` (or a
0600 systemd `EnvironmentFile=` holding `RPC_LOGIN`), never on a command line, in shell history or in a unit file's
`Environment=`, and to firewall it to SatBooth's host. When a pass finds the wallet's base address changed, or after
a failed re-check (a replaced wallet, for example), the watcher pauses rather than read a wrong wallet's answers (§9).

On Linux the service locks all its memory (`mlockall`) and disables core dumps and ptrace before it loads a key, so
keys do not reach swap or a dump. It refuses to start if it cannot lock, unless the operator sets `allow_swap` for a host
whose swap is encrypted or absent (`design.md` §3, "Operation"). The checkout listener is the only public one; the
merchant API is on a private address (§5).

Out of scope: a timing or cache attacker on the same host. btcec's signing (`ScalarBaseMultNonConst`) and the
NIP-44 ECDH (`ScalarMultNonConst`) use variable-time scalar multiplication. SatBooth runs on a host it does
not share with other tenants.

## 1. Invoice-creation floods

Each invoice is a write to the hub.

- **Hard global limits in SatBooth:**
  - creation rate (token bucket);
  - a cap on open invoices;
  - short expiry (15–30 min);
  - amount bounds;
  - a description length cap;
  - a **daily budget**.

  As built, the defaults are 1 creation a second with a burst of 10, 1,000 open invoices, 5,000 attempts a day, a
  smallest amount of 1 sat, and an invoice expiry of 20 minutes (15 to 30). The daily amount and the largest
  amount have no default: the operator sets them. The daily figures count attempts in the UTC day and are
  read back from the store after a restart. There is no description length cap to configure: the description is
  fixed text plus the order id.
- **Atomic creation:** creation is serialised, so two requests can't both pass the open-invoice cap. (Built: the
  limiter and the counts are used under one mutex in `lnpay`.)
- **Capped share for `new`:** two classes, `known` (an account with a past paid order) and `new`.
  - `new` is capped at a small share of the open-invoice cap; `known` may use everything else.
  - `known` is verified against SatBooth's own payment records, never taken from the caller's say-so.
  - Becoming `known` costs a real paid order, so each Sybil there costs the attacker money.
  - As built, `new` gets 20 % of the open-invoice cap by default (`new_share`). Unused `new` slots are lent to
    `known` accounts, and a flood of `new` requests is still capped at its share. The trade-off: `known` traffic
    can now fill all of `max_open` and leave no room for `new` customers. Becoming `known` takes a paid order, so
    this costs an attacker money. A cap below 5 cannot be split with the default share, and is refused at startup.
- **Per-account cap (plan 6.1).** Each `account_id` may hold at most `max_open_per_account` open invoices (default 5,
  or `max_open` when that is smaller; 1 to `max_open`), in both classes, on top of `max_open` and the `new` share.
  It is not a defence against Sybils, whose account ids cost nothing (they still meet the global caps first); it
  bounds what one account, a paid `known` one in particular, can hold. It is checked in the same mutex as the other
  counts (`limits.Admit`, check 7, before the rate token, so a refusal burns no token), and the customer sees "Busy,
  try again." The account id is the merchant's app's own (§3 of `design.md`, "Who calls SatBooth").
- **Residual (plans 6 and 6.1): `known` can starve `new`.** Because unused `new` slots are lent to `known` accounts,
  **a `known` burst can still fill `max_open`, but each account holds at most `max_open_per_account` (5 by default)
  invoices, so filling it costs one paid order per 5 slots**, and no new customer gets a slot until those invoices
  expire.
- **On-chain addresses are outside these limits.** An address is a write to the merchant's wallet, not to the hub:
  one per order, made on the first "pay" press, never reused. Orders come only from the merchant's app, so their
  number bounds the addresses; unpaid ones lengthen the run of unused addresses, which logs an `ALERT` past
  `gap_alert` (default 1,000; `docs/bitcoind.md`, "The merchant's wallet").
- **On the public side:** an invoice is created only after an explicit "pay" action, behind a CAPTCHA-type
  challenge plus WAF rate limits. There is one open invoice per order, reused rather than created anew on
  every page reload.
- **Never create amountless Lightning invoices:** a node accepts any amount for one.

## 2. Status-polling floods

Browsers poll SatBooth, which answers from its own state. Node-side reconciliation runs at a fixed rate,
so node load doesn't grow with the number of attackers. As built, the reconciler makes 5 lookups a second in
total, however many invoices are open or pages are loaded; a notification only moves an invoice to the front of
that queue.

## 3. Fake or replayed "paid"

An order is paid only when all of these hold:
- SatBooth's own check of the rail shows enough received (`lookup_invoice`/`list_transactions`; the
  confirmed amount at the invoice's address or subaddress);
- the invoice or address was created for that order (1:1, stored at creation);
- the order is marked consumed atomically: one payment, one fulfilment. As built, "paid" is one ledger transfer
  whose id is derived from the payment hash, so a payment seen twice (a notification and a poll, a restart) is the
  same transfer, and the webhook event for it exists once (`design.md` §3).

Residual (plan 5, Lightning): a node can settle an invoice after its expiry (LDK accepts payments for about 2 hours
after it, depending on block-header times; `design.md` has the source), after SatBooth's closing lookup has expired
the order. SatBooth keeps looking such an invoice up for 24 hours after its expiry (about 48 hours if SatBooth
restarted inside that time) and records a late payment (with an `ALERT`; `order.paid` then follows
`order.expired`). Two cases stay unrecorded: a settlement after that watch, far outside LDK's window, and one made
while SatBooth is down if it stays down past the invoice's expiry plus 24 hours (after a restart it watches only
invoices whose expiry plus 24 hours has not passed). The money is at the hub in both; they are an alert-and-manual
case for the operator. One more case is recorded but not always completed: if a late payment's transfer is written
and the process then crashes before its `order.paid`, the next start completes it (the `account_paid` fact, then
`order.paid`) when that invoice was closed in the 7 days before the start (`lnpay.HealWindow`, plan 6). A start more
than 7 days after the closure completes nothing: the ledger is right and the order reads `paid`, but `order.paid`
and the `account_paid` fact stay missing. The late-payment `ALERT` is logged before the record, so the operator
always has an alert naming that order.

Lightning, the hub's network and the invoices it makes (plan 12). The configuration names the network of the Lightning
side (`nwc.network`: `mainnet`, `testnet`, `signet` or `regtest`), and `onchain.network` and `payout.network`, when
their blocks exist, must equal it. Every invoice that `make_invoice` returns is checked before it is stored or shown:
the payment hash has the shape of one; the amount the hub reports is the order's; and the invoice's own text, exactly
as the hub returned it (an uppercase invoice is refused, nothing is normalised), is a well-formed BOLT 11 invoice
for the configured network (its prefix, its bech32 checksum) that carries an amount in its human-readable part equal
to the order's amount. An invoice that fails any of these is not stored and not shown, the slot is given back, the
caller gets the unknown outcome as for any bad answer, and `ALERT: make_invoice answered with an invoice for another
network or amount` is logged without any part of the invoice. SatBooth also asks the hub for its network at every
start (plan 14): the answer of `get_info` on the invoice connection, and on the payout connection when the `payout`
block exists, must name the network of the configuration (`nwc.network`, and `payout.network`, which equals it), or the
start is refused before the network is recorded and before anything is served. An answer that names no network is
refused too: Alby Hub states its network only to a connection that holds the `get_info` permission (the hub's form
shows it as "Read your node info"), so `satbooth-pair` asks for it in both links, and an operator who unticks it in
the hub is refused at start with a message that says how to pair. A network name is printed only if it is one of
`mainnet`, `testnet`, `signet` and `regtest`; no other string of the hub's reaches a log or an error. With the scope,
`get_info` discloses to the connection the node's alias, colour, public key, network, block height and block hash
(getAlby/hub, `nip47/controllers/get_info_controller.go`). They are public or in every invoice, and they add no way to
move money. The check compares the hub's own statement with the configuration, so it catches a configuration that
names another network than the hub's and a hub that was set up for another network. What it does not prove: the hub
states what its own configuration says (it reports its `NETWORK` setting as it is, with `bitcoin` mapped to `mainnet`,
so a hub set to `testnet` that runs a signet node states `testnet`), and a hub that lies about its network already
holds the funds (§0). The answer is authenticated as coming from the paired wallet key (the NWC client accepts a
response only after the kind, the signature, the author, the `p` tag and the NIP-44 decryption with the conversation
key have been checked), so a relay cannot forge it. The check runs once, at start: nothing asks `get_info` again while
SatBooth runs, and a hub restarted on another network with the same wallet key is caught only by the per-invoice check
(its invoices are refused, not stored and not shown, with the `ALERT` above) and by the next start. The per-invoice
check above stays on every `make_invoice` answer as the second line. Invoices stored since that check were checked
when they were stored, and are not checked again; an invoice stored by an earlier version stays open until its expiry
(and is watched afterwards as the Lightning rules describe) without having been checked. No other path takes an invoice
text from the hub. The payment hash SatBooth stores is the one the hub reports in its answer, not one read out of the
invoice text (SatBooth decodes no tagged field), and only a hub that lies can make the two differ. What is not checked:
the invoice's tagged fields (it decodes none, so its destination and its description are the hub's). Residual: a hub on
the right network that lies about everything else already holds the funds, as the hub is trusted with them (§0).

A data directory belongs to one Lightning network. The case is an operator who reuses a data directory made under one
network (a regtest or signet test) for another (mainnet): the invoices and orders stored under the first would be read
under the second. The store records `nwc.network` (`store_network`, written once and never changed) at the first start
that passes all its checks, just before the first worker and server start; a start that is refused for any reason
records nothing. A start under another network than the recorded one is refused at once, before anything else uses the
store, with `this data directory belongs to <network>`. A store made before this check has no record. If it holds no
invoice, the first start that passes its checks records the configured network, as nothing else can be known. If it
holds invoices, whose text states their network, the newest one must not be for another network (the payment path's
own network check, applied to the other networks; an invoice that passes for none states no network and refuses
nothing), or the start is refused with `this data directory holds invoices of another network` and nothing
is recorded: a store whose newest invoice states a network is never recorded under another. Older invoices are not
read. The record is permanent and it does not prove the network: it is the configured `nwc.network` as the start
passed it, and `monero.network` is Monero's own and is not compared. A wrong `nwc.network` no longer starts when the
hub states its network: the start is refused by the check above, so nothing is recorded. Only a hub that states a
network and is wrong about it, which already holds the funds, gets a wrong record; every Lightning invoice is then
refused by the per-invoice check, so no Lightning order can be paid. Stop the service, move the directory aside and
start with a new one. With a `monero` block, Monero orders may have been paid meanwhile: they stay in the directory
that was moved aside, and the merchant handles them by hand, as for records past the watch window.

On-chain (plan 6), an order is paid when, for its address (made for that order and stored before it was shown):
- each payment output that counts was first seen by SatBooth before the order's expiry (or is an RBF replacement that
  inherits such a time, below), and is mined with the confirmations of the order's tier (by `onchain_sat`; 1, 3 or 6
  by default), or in the mempool when the tier is 0;
- SatBooth's own `gettransaction` says so, not a notification: the watcher polls bitcoind at a fixed rate;
- the ledger holds one transfer per such output, with an id derived from (`txid`, `vout`, epoch), and their sum
  reaches the amount. A payment seen twice, or after a restart, is the same transfer; `order.paid` exists once.

A recorded output that is no longer mined (a reorg, a double spend; at a 0 tier, one no longer in the mempool or a
block) gets a reversing transfer, and the order reads
unpaid again if the rest no longer reaches the amount; `order.reverted` goes out only when the order, both rails
together, stops reading paid. At a 0 tier a record whose payment dies in a pass after that pass's scan, when its
reversal would turn the order unpaid, is reversed one pass later (its fee bump may be listed only then): for that pass
the order still reads paid on a payment already gone, which a 0 tier's trust in the payer already covers. The same
holds while bitcoind's wallet is behind its node (below). A payment first seen at or after the expiry is `late`:
recorded in the ledger once
confirmed, never counted, and never fulfilled automatically; a person credits or refunds it. Overpaid and underpaid
orders also wait for a person.

Residual (plan 6, on-chain, Ruling G): **a reorg of the largest tier's confirmations (6 by default) or more after an
order was paid is not detected, because polling stops at `SettledDepth`.** A payment recorded that deep, against a
confirmation count `gettransaction` had just verified, is stored as settled and not polled again, also after a
restart (a payment whose last poll failed, or that a pass did not reach, is not settled). Bitcoin Core treats such depth as settled;
watching every output for its whole window would cost a `gettransaction` per output per pass with no bound.

**Confirmations are bitcoind's own count** (plan 14, as the Monero rail's below). A pass reads the chain tip once and
the stored scan position carries that tip, but bitcoind's tip can go down afterwards: a reorg onto a chain with
fewer blocks, or a bitcoind that was killed right after blocks were mined and came back with a shorter chain
(`scripts/qualify.sh`, the comment of the SIGKILL helper records it). A count taken from the scan position alone
could then exceed the transaction's real confirmations, record a payment early, or settle an output for good that is
not that deep. SatBooth therefore applies one rule where an order's outputs are loaded (`orderViews` in
`onchain/settle.go`; the permanent settle, `settled`, uses the same helper, `boundOut`): a mined output counts at most
the `confirmations` that the `gettransaction` of the last completed pass answered for that transaction, and only when
that answer's block, by height and block hash, is the block its mark names; a mined output that is not yet recorded and
has no such reading counts zero (on a tier of 0 a zero count does not stop a record; there every unsettled on-time
output is read in the pass or its order is skipped, and a late output needs at least 1 confirmation). So nothing
on-chain is recorded, as paid or as late, or settled for good, without a reading in the same pass, and the paid
decision, the permanent settle and the count that the API, the checkout page and the status show are one count. A
recorded output's count is not bounded: it decides nothing, and a settled output is not read again. The bound is kept in
memory for one pass: nothing is stored for it.

A late output (first seen at or after its order's expiry) of an order past its watch window is no longer among the
watched outputs. When its last mark is in the mempool or mined and it is not yet recorded, it is read again in the pass
that is about to settle its order (the order was touched: a payment to its address was found, or an on-time output is
still followed). It is then recorded late from that reading, or not at all when the transaction is gone (its mark
becomes dead and no `order.late` is sent). A late payment still in the mempool when its window closes, and mined later,
is recorded late (`order.late` and its `ALERT`) when its order is next touched and it has its tier's confirmations; a
dead one is not read again. A late payment is never counted as paid and never fulfilled automatically; it goes to a
person. The cost: no extra call in the normal case; such an output costs one `gettransaction` a pass in which
its order is touched, two calls while it is unconfirmed (with `getmempoolentry`), and six in the pass that finds it
gone. If the extra read is refused or answers badly, that output is not recorded in that pass, the order reads stale
and its on-time payments are still recorded; if bitcoind's mempool is still loading, it is not recorded in that pass
and the order is not marked stale; a transport failure ends the whole pass, which records nothing.

What stays: **a reorg deeper than the settled depth after an output was settled** (Ruling G, above), and the count shown
for an output already settled for good or recorded and not read in the last pass, which is the scan position's;
**a late output of a past-window order that is never touched again**, which is never read and never recorded, and a late
output recorded past the window, which is not read again; **a late payment that arrived after the window**, which has no
mark, is not read and not recorded (its after-window `ALERT` names its order); and the count of an output that is
unrecorded, which shows zero until a pass has read it (after a restart, or when its read failed, and then the order
reads stale).

Residual (plan 6.1, on-chain, decision 4): **a tier of 0 confirmations trusts the payer for that amount.** A payment
counts as soon as SatBooth sees it in bitcoind's mempool, and since Bitcoin Core 28 full RBF is the default (*source*:
the Core 28.0 release notes, `mempoolfullrbf` default 1), so the payer can replace it with a payment to themselves
until it is mined. SatBooth then reverses the record and sends `order.reverted` (when the order, both rails together,
stops reading paid), but whatever was fulfilled in between is lost. The operator accepts that risk per tier, by
choosing 0 for small amounts; the defaults are 1/3/6. A record from the mempool settles only once mined
`SettledDepth` deep (at least 1). A late payment is never recorded from the mempool, whatever the tier (Ruling 6.1-10:
it needs at least 1 confirmation), so a person never acts on a late payment the payer can still replace.

Residual (plan 14, on-chain): **for up to 60 seconds (plus the passes) while bitcoind's wallet is behind its node's
mempool, a 0-tier order reads paid on a payment that was already replaced.** The wallet learns of mempool changes
through a queue and can lag its node by seconds on slow storage. After a fee bump the node has dropped the original and
holds the replacement, while the wallet shows neither a conflict nor the replacement. A pass that finds a payment
missing from the node's mempool therefore asks the node which mempool transaction spends its inputs
(`gettxspendingprevout`, a node call): another one means the payment was replaced and the wallet has not processed that
yet, so the pass marks nothing for that payment, counts it as zero if it is not recorded yet (a recorded one keeps its
record), and logs one `WARN` line; the order's other payments go on as in any pass, and its final event waits. The
payment is judged once the wallet shows the conflict (Core's wallet puts the id of any mempool transaction that spends
the same input in `mempoolconflicts`, also one that is not its own), or 60 seconds after SatBooth first found the
payment replaced in the node, whichever is first: then the node's word is taken and the payment is marked gone. (Core's
wallet drops a conflict again when the replacement is itself replaced, so a payer who keeps replacing could otherwise
renew the wait.) So one fee bump is not reported reverted because the wallet is behind, as long as the wallet shows the
replacement within those 60 seconds. When it is behind for longer, the payment is marked gone, and when the wallet has
still not listed the replacement one pass later: on a 0 tier the order gets `order.reverted` and, when the wallet lists
the replacement, `order.paid_again` with its `ALERT`; on a tier above 0 across the expiry, `order.expired` and then the
paid-after-final `ALERT`. A false event costs nothing; a paid order on a gone payment does, which is why a longer wait
is not allowed. The 60 seconds count from the first find, also when the payment comes back into the mempool meanwhile or
was marked gone before; they are checked when a pass reads the payment and again finds it replaced (the payment is
marked gone at the first such pass at or after them, and reversed one pass later, so the real limit is 60 seconds plus
two passes and the length of one pass), and a pass that cannot read it (bitcoind's mempool still loading, a failed call)
does not count, so there is no wall-clock limit while reads fail (those states have their own log lines). Each of the
two waits, the 60 seconds and the one pass after the mark, is granted once per paying output (one transaction output of
the order) in a running process, and is kept in memory until the process ends: an output that left the mempool, came
back and leaves again gets no new 60 seconds and no second hold. What is left of its first 60 seconds still applies, so
it is marked gone and reversed in the pass that finds it gone only once those are over. A fee bump of such an output
that lands between a pass's scan and its read is not held either: the order reads `order.reverted`, then
`order.paid_again` with its `ALERT`. Each restart grants each wait once more. A limit of a 0-confirmation tier: every
new paying output, such as the replacement of a fee bump, has its own two waits, so a payer who keeps putting in
replacements that pay the order and removing them again can keep the order reading paid, without `order.reverted`, while
no paying transaction is in the mempool most of the time. Per order the bound is 60 seconds plus two passes since a
paying output of the order was last in the node's mempool, and it ends when one of the transactions confirms. A payment
really double spent is reported reverted when the wallet has seen the conflict or the 60 seconds are over, plus the one
pass above, not when the node has. The order reads paid meanwhile, and no `ALERT` is raised for the wait: for up to
those 60 seconds, that payment is neither recorded nor reversed and the order's final event waits (no "no final event"
`ALERT` either); the only signal is the `WARN` line once per pass, and the order reads as it did. (A whitelist that
lacks the call fails the startup check, or ends the pass with the usual `ALERT` for a failed call.) A payment that
nothing in the mempool spends (evicted or expired) is marked gone as before. The money is not at risk beyond the 0
tier's trust: a 0-tier payment is already one the payer can replace. A payment whose block was reorged out is not held
for the wallet (it is marked gone as before), so a tier above 0 is not affected.

Residual (plan 6.1, on-chain, decision 8): **an RBF replacement can inherit an on-time first-seen time.** It does so
only when bitcoind lists the replaced transaction in its `walletconflicts`, every output of that transaction to the
address was first seen before the expiry, the replacement pays the address exactly that transaction's total, and no
other on-time payment of the order, or a payment counted earlier in the same scan, could confirm beside it (Rulings
6.1-S and 6.1-U). So a set of payments that can
confirm together never counts more than was committed on time (a seeded property test checks it); a replacement
that pays the address more or less is a new, late payment. The rule errs toward late (two independent on-time parts, one bumped after
the expiry; or cancel, pay again, bump), and a person credits those. It is decided once, when the replacement is first
stored, so a node that heard the replacement before the original gives it its own time; an inherited time is never
after SatBooth's clock (Ruling 6.1-T). It inherits only when it is first listed inside its order's 7-day watch window
(decision 9 of plan 6.1): without that cap, an on-time payment kept unconfirmed at a fee that
never confirms (the stuck set has no calendar cap, and the payer's node rebroadcasts it) could be bumped at the exact
amount weeks after the expiry and count on time, a free option on the quote (with fiat prices, on the exchange rate).
The cap closes that option. Its cost, toward late: a customer whose payment is still stuck at the window's end and who
bumps it then gets a `late` payment that SatBooth does not poll or record (its after-window `ALERT` is the signal),
and a person credits it.

Residual (plan 6, on-chain, Ruling L): **a payment sent on time but first seen after a bitcoind or SatBooth outage
that spans the order's expiry reads `late` and waits for a person.** First-seen is SatBooth's clock, never bitcoind's
`timereceived`, so an outage cannot be told apart from a late payment. Nothing is fulfilled
automatically; the cost is a person crediting it by hand.

Residual (plan 6, on-chain, Ruling Z2): a late payment reads `late` in the API as soon as it is seen, before it
confirms, and it can still be replaced or double spent; `order.late` and its `ALERT` wait until it is recorded at its
tier's confirmations, at least 1 on every tier (Ruling 6.1-10). Whoever refunds it from the API state alone waits for
its confirmations first. A payment after the watch window is never recorded and sends no `order.late` (its own `ALERT`
is the signal), and a late payment is not watched past the window (see "Confirmations are bitcoind's own count").

Residual (plan 6.1, on-chain, decision 2): **a stuck payment.** An on-time payment still unconfirmed when its order's
7-day window closes no longer leaves the order as it is: the order reads `underpaid`, gets `order.underpaid` and an
`ALERT` (once its Lightning side is done), and the payment stays tracked past the window until it is mined (then
`order.paid` after `order.underpaid`, Ruling S) or gone (then, once the order is final and nothing of it stands in the
ledger, SatBooth stops polling it with an `ALERT`, none when the order reads paid by its events or when a fee bump
that inherited its first-seen time carries it on, and a later scan that lists it mined brings it back; a rebroadcast
seen only in the mempool does not, Ruling 6.1-R; the order still reads `underpaid`, as its event says, never
`expired`). The bump is matched to its dead original by that first-seen time, to the second, because the link is
not stored (storing it needs a migration): an unrelated on-time payment of the order first seen in the same second,
live while the other dies, also silences that `ALERT`. Only the `ALERT` is lost; the money, the events and the API
are unchanged. Fulfil an `underpaid` order only after a person has looked. The API and the page read an on-chain order's
`expired` and `underpaid` from its events, never from the clock alone: they lag the
expiry by up to one pass (longer while a pass holds the event for a fee bump the next scan may list: a payer who keeps
replacing a payment can keep an order's final event waiting for as long as they pay the fees, within the order's watch window, on
either rail of a mixed order, and the paid state is not affected), the order never
reads `expired` while an on-time payment is live, and one whose settle keeps failing past its window reads `new` for
good, with `ALERT: an on-chain order left its watch window without a final event` as the signal. The cost
is one `gettransaction` and one `getmempoolentry` a pass per stuck payment, one more `getmempoolinfo`,
`getmempoolentry` and `gettransaction`, and a `gettxspendingprevout`, in the pass that marks it gone (six calls then; a
payment the wallet is behind on costs the same each pass until the wallet has caught up), the same two calls a pass for
one already marked gone, and one scan of the stored payments a pass. There is no calendar cap on the tracking (Ruling
6.1-3); a fee bump of it inherits its first-seen only inside the window (decision 9). Read in Core 32.1
(*source*; `docs/rules-onchain.md`, "The watch window",
has the lines): the watch-only wallet itself resubmits a received unconfirmed transaction to the mempool every 12 to
36 hours and at each load (`src/wallet/wallet.cpp:2147-2180`), but the mempool still drops it `mempoolexpiry` hours
(default 336) after it entered (a rebroadcast through bitcoind while it is there does not reset that time), and a pass
in the gap before it comes back marks it gone. So it is normally polled for at most about 336 hours after it last
entered bitcoind's mempool; a pass that misses the gap, or a transaction that left and came back inside the window,
extends that. A mined payment whose ledger record has other details never settles and is polled until it is put
right by hand.

Note (plan 6, on-chain, observed): bitcoind flushes its block index and chainstate on a timer, so a crash or `SIGKILL`
can bring it back at a lower height (`qualify/REPORT.md`, "Index"). SatBooth sees the vanished blocks as a reorg: a
recorded payment it still polls is reversed (`order.reverted` if the order stops reading paid) and recorded again,
with `order.paid_again`, once bitcoind has the blocks again; if the cursor block is gone, the scan re-anchors (below).
A payment already recorded and verified `SettledDepth` deep is not polled again, in any process (Ruling G).

Residual (plan 6, on-chain): if bitcoind can no longer list from the block the scan's cursor names (`Block not
found` after a reindex, or a pruned node that cannot read a detached block), SatBooth re-anchors the scan at the
current tip and logs an `ALERT` that says so, rather than stopping discovery for good. Payments already tracked keep
being refreshed by `gettransaction`. A payment first mined between the lost block and the tip, and never seen in
the mempool by SatBooth, is not discovered; the operator reconciles that window by hand from the merchant's wallet.
As built, any JSON-RPC error (not a transport failure) to that `listsinceblock`, and an answer too large to read
(over 8 MiB, after a long outage on a busy wallet), re-anchors, so the `ALERT` can also follow an error that lost
nothing (a wallet that is not loaded, for example: then the retry from the tip fails too and the cursor stays). A
timeout never re-anchors.

Residual (plan 6.1, both rails, Ruling 6.1-W): an order paid on both rails logs `ALERT: an order was paid on both
rails` whichever rail pays second, at any depth, for any recorded on-chain payment (counted or late) beside a
Lightning payment, so a person refunds one rail. It is logged once per order per process (again after a restart). It
is lost only when the on-chain record is already settled `SettledDepth` deep, so no pass reads the order, and the
Lightning payment is completed by a path that does not tell the on-chain rail: the startup completion of a closed
invoice whose call to the rail fails, or the reconciler's heal of a closed invoice after a crash between its transfer
and its event. The ledger holds both payments, and the API and the page show the extra either way.

Monero (plan 8, written before the code, **as built**: `monero`, `rail`, `store`'s Monero tables). An order is paid on its Monero rail
when, for its subaddress (made for that order, its index recorded before it is shown, never minor index 0, never
reused):
- each payment that counts was first seen by SatBooth before the order's expiry (SatBooth's clock, never
  wallet-rpc's `timestamp`), is not locked by its sender beyond the normal lock (below), and is mined with the
  confirmations of the order's tier (by `monero_piconero`; 1, 3 or 10 by default), or is in the tx pool when the tier
  is 0;
- SatBooth's own `get_transfers` (`in` and `pool`) and `get_transfer_by_txid` say so, not a notification: the
  watcher polls wallet-rpc at a fixed rate;
- the ledger (in piconero) holds one transfer per such payment, keyed by (`txid`, subaddress), and their sum reaches
  the amount. A payment seen twice, or after a restart, is the same transfer; `order.paid` exists once.

Monero reorgs and drops (plan 8, as built) reverse as on-chain: a recorded payment that is no longer
mined (a reorg), or, at a 0 tier, one that left the tx pool or is seen there with `double_spend_seen`, gets a reversing
transfer, `order.reverted` when the order, all rails together, stops reading paid, and `order.paid_again` when it is
mined again. On a tier that needs a block, a pool entry with `double_spend_seen` is not reverted: it stays a pool
payment with one `ALERT`, since the block decides. wallet2 itself drops the payments above a fork from its list and
refuses a reorg deeper than 100 blocks by default (*source*: monero-project/monero tag v0.18.5.4,
`src/wallet/wallet2.cpp:4486-4491`, `:3454-3457`; the default `ORPHANED_BLOCKS_MAX_COUNT` 100,
`src/cryptonote_config.h:77`, set at `wallet2.cpp:1221`), so SatBooth's store is the only record of first-seen and of
what disappeared. The late, partial and over-payment rules of plan 6 and 6.1 carry over unchanged (plan 8, decision 9);
Monero has no RBF, so the first-seen inheritance of plan 6.1's decisions 8 and 9 has no Monero case. The residuals above
apply to Monero as written for on-chain: **a reorg of the largest tier's confirmations (10 by default) or more after an
order was paid is not detected** (Ruling G), and **an on-time payment first seen after a wallet-rpc, monerod or SatBooth
outage that spans the order's expiry reads `late`** and waits for a person (Ruling L). Monero's detection lag is
wallet-rpc's auto-refresh (20 s by default, *source*: `src/wallet/wallet_rpc_server.cpp:64`) plus SatBooth's poll, so a
payment sent seconds before the expiry can also read `late`. A tier of 0 trusts the payer as on-chain: Monero has no
RBF, but a conflicting spend can still win the race to a block (*inferred*).

**Confirmations are the wallet's own count** (ruling 8-A2). SatBooth's scan position never goes down, and a wallet can
sit below it: restored and rescanning, or moved by its operator to a monerod on another fork whose tip is lower (a
normal reorg does not shorten the chain: monerod switches only to more cumulative work). Each pass stores the wallet's
height beside the scan position, in the same transaction, and every confirmation count (recording, the settled depth,
the API) is taken from it, never from the scan position. A payment mined on the shorter fork therefore has exactly the
confirmations wallet-rpc shows, and a rescanning wallet makes payments wait rather than count early. While the wallet
catches up, a payment whose transaction it does not know in a pass is not newly recorded in that pass from an older
mark (ruling RU): that mark was counted against the lower height and could otherwise give a false `order.paid`.

The on-chain rail takes its counts the same way from bitcoind's own answer (plan 14, "Confirmations are bitcoind's own
count" in the on-chain section above).

**A pass that reads a changing wallet writes nothing** (ruling 8-A6). wallet-rpc's height can go down
between two of SatBooth's calls (a refresh that meets a reorg onto a chain with fewer blocks crops the wallet's chain,
*source*: `src/wallet/wallet2.cpp:3446-3459`, `4475-4482` at v0.18.5.4; so do a daemon switch and a rescan), and a
wallet file replaced mid-pass answers "not found" for every transaction. A pass therefore writes nothing while it
reads: its new payments, its scan position with the wallet's height, and its marks wait in memory. After its last call
it reads the height and the wallet's identity again, and, under the settle lock, checks that no press or identity read
saw another wallet meanwhile (a generation counter). A lower height, another identity or such a sighting discards the
pass (a WARN with the reason; an `ALERT` at the third in a row) and the next pass reads the wallet again; otherwise
everything is written in one transaction and only then settled. The pass keeps that lock through all its settles, and
a sighting of another wallet takes it to move the counter (ruling S2), so a sighting lands before the check (the pass
is discarded) or after the settles, never between them. A new payment's first-seen time is SatBooth's clock when the
pass's listing first returned it, not when the pass wrote it (ruling S3), so a slow pass never makes an on-time payment
late; and the orders the write touched are recorded in the same transaction as pending settle until they have settled
(ruling S4), so a crash right after the write cannot leave a late payment found already deep unsettled once its
window has closed. An order skipped because a sibling payment's call failed is recorded too when the write holds a
new mark for it (ruling T1), and a settle that reverses a late record still writes its `order.late` (ruling T3), so
neither a failing sibling nor a reorg during crash recovery loses a late payment's event once the window closes. A
late payment whose call failed in every pass of its window, and so was never observed, gets its own `ALERT` when the
window closes (§9). Every confirmation count is also at most the count
wallet-rpc gave in the same answer as the payment's mark (`set_confirmations`,
`src/wallet/wallet_rpc_server.cpp:163-168`), so a payment seen on a shorter chain that wins back within the pass is
neither recorded nor stops being watched on the pass's earlier height. **Remaining gap:** a switch to another wallet
and back (A, B, A) within one pass, with no press in between: both identity reads see A and nothing moves the counter,
so the other wallet's "not found" answers are written as dead marks and the order's payments are reversed (a false
`order.reverted`, then `order.paid_again` once A lists them again). So does a press that saw B whose sighting had not
yet moved the counter when the pass took its lock (B came and went between the press and the pass's last identity
read): it pauses the rail after the pass's settles. Identity reads cannot see it; not trusting a single
"not found" (requiring it on two passes) would. It needs the wallet file switched twice within seconds by someone with
access to wallet-rpc's private link.

**The two watched rails settle one at a time** (ruling 8-A1). Each rail's settle reads the other rail's paid state and
then writes `order.paid_again` or `order.reverted` from it; both rails hold one shared lock for the whole of each
settle, so the other rail's state cannot change in between (its own settle is the only writer). Without it, a
reversal on one rail landing inside the other's settle gave an `order.paid_again` that no payment backed, or, when
both rails' payments reverted together, no `order.reverted` at all. Lightning needs no such lock: it writes only
`order.paid`, the same idempotent event, and a Lightning payment never reverts. A status read (the API, the checkout
page) takes the same lock (ruling R1): a settle used to record a new payment before it reversed an old one, and a read
between the two summed both, so an order whose reorged-out payment was replaced by another could read `paid` for a
moment with no `order.paid` behind it. Since ruling S1 a settle writes all its ledger records (the new payments'
forwards and the old ones' reversals) as one transaction, before its events: a settle that ends early (an error, a
crash) leaves none of them, so neither a status read nor the next settle ever sums a payment with the one it replaces
(which used to publish a false `order.paid` on restart, then `order.reverted`). A reversal the ledger refuses because
a record with its id and other details exists fails the settle with nothing written (ruling TU1): leaving it out and
writing the rest would record the replacement beside the payment it replaces. A rail reads its peer without the lock, from inside its own settle, which
holds it. A reversal and its `order.reverted` live in two databases (the ledger and the order store); a crash or a store
error between them is healed by the next settle of the order on either rail, which first adds the missing
`order.reverted` for both rails' reversals, read from the ledgers alone (ruling R2). Without that, the other rail could
settle first, miss the `order.paid_again` of its own payment and reuse the lost event's number for its own reversal,
merging two paid-to-unpaid cycles into one.

**A sender's `unlock_time` makes the payment late by rule** (plan 8, decision 6; written before the code, as built). An
output is spendable 10 blocks after its block (`CRYPTONOTE_DEFAULT_TX_SPENDABLE_AGE`, *source*:
`src/cryptonote_config.h:49`), about 20 minutes at 2-minute blocks (`DIFFICULTY_TARGET_V2` 120 s, `:80`), but a sender
may lock it longer with `unlock_time` (a height, or a timestamp). A payment whose `unlock_time` is above 0 and above
the wallet's height at its first sight plus 10 is never counted, whatever its first-seen time. A value of
`CRYPTONOTE_MAX_BLOCK_NUMBER` (500,000,000, `:40`) or more is read as a timestamp, not a height (*source*:
`is_tx_spendtime_unlocked`, `src/wallet/wallet2.cpp:7451`), so a timestamp lock always exceeds the bound. Such a
payment is recorded as late once mined, gets `order.late` and an `ALERT`, and a person decides. It is judged against
the height stored when the payment was first seen, never the current tip, so its classification never flips as the
chain grows. Residual: a payer whose wallet sets a lock by mistake waits for a person, and the merchant cannot spend
the money before the lock ends whatever the person decides. As built, the case is rare at v0.18.5.4: wallet-rpc's
`transfer` refuses a non-zero `unlock_time` with -50 (*source*: `src/wallet/wallet_rpc_server.cpp:1266-1271`;
*observed* on regtest), and monerod's pool refuses to relay such a transaction (*source*:
`src/cryptonote_core/tx_pool.cpp:185-191`), so only a miner can put one in a block. The rule stays as a defence, tested
with the in-memory wallet-rpc (`monero/monerotest`), not on regtest.

The both-rails rule (Ruling 6.1-W) covers Monero beside either bitcoin rail (plan 8, as built):
`ALERT: an order was paid on both rails` fires on a record when any other rail reads paid or holds money (received,
late or a record), since money on two rails needs a person. Each watched rail logs it once per order per process, so
money on the on-chain and the Monero rail gives two lines (*observed*, `qualify/REPORT.md`). On an order with both an
on-chain and a Monero amount, neither rail writes a final event while the other is still being paid or its view is
stale or holding its event for an output the wallet has not processed (rulings 8-T7d and 8-T7e): a stale rail may hold
money it cannot see, and SatBooth does not invent a final
event it cannot know. Such an order still held at its window's end gets the rail's left-the-window `ALERT`, and a
person decides.

NWC notifications and webhooks are hints only. Also:
- Never return a Lightning preimage, or any other secret field, from any API. Every response is built
  from an allowlist of fields.

## 4. Money out: refunds and payouts (the highest risk)

SatBooth pays only Lightning refunds, and only what this section allows. Everything else is refused at the boundary
with a fixed error code, or, for a state that should not exist, logged as an `ALERT:` line with nothing changed.
There is no automatic repair path.

**Built (plan 9).** This section is implemented as written by `approval`, `lninvoice`, `refund`, `store`, `api`,
`service` and `cmd/satbooth-approve` (design.md §3, "Lightning refunds, as built (plan 9)"), and qualified against
real hubs on regtest (`qualify/REPORT.md`, "Lightning refunds").

**Built (plan 11).** The void's rows V1a to V1e below are implemented by `refund` (`refund/pay.go`, `Void`) and
`store` (the reasons `cancelled` and `ledger_paid`), as `design.md` §3, "The void, as built (plan 11)" states, and
the void of a claimed refund is qualified on regtest (`qualify/REPORT.md`, `TestRefundBudget`).

### Payout connection: least privilege

- Payouts use their own NWC connection with a hub-enforced lifetime budget (renewal `never`), raised by a person in
  the hub when it runs low. The hub counts every settled or pending payment, including fees and the fee reserve,
  since the connection was created (`db/queries/get_budget_usage.go`). SatBooth alerts before the budget runs out.
  The budget is the hard cap on all payouts.
- The connection is granted the scopes `pay_invoice` and `lookup_invoice`, and `get_info` (plan 14: with it the hub
  states its network, which SatBooth checks at every start; the method adds no way to move money, and it discloses
  to the connection the node's alias, colour, public key, network, block height and block hash). The hub adds two
  groups of methods:
  (a) `pay_keysend`, `multi_pay_invoice` and `multi_pay_keysend`, which it always grants with `pay_invoice`
  (observed, `qualify/REPORT.md`); SatBooth never calls them; (b) `get_info` and `get_budget`, which it always
  grants with no scope (`nip47/permissions/permissions.go:195-197`, getAlby/hub `921003b`). The startup check
  tolerates (a) and (b) and refuses a connection with any method outside `pay_invoice`, `lookup_invoice`, (a) and (b).
- The invoice connection can't pay. This is checked at start, deny by default: the invoice connection's `get_info`
  must list methods, must hold `make_invoice` and `lookup_invoice`, and must list nothing outside what the hub reports
  for SatBooth's own invoice pairing (`make_invoice`, `make_hold_invoice`, `settle_hold_invoice`,
  `cancel_hold_invoice`, `lookup_invoice`, `list_transactions`, `get_info`, `get_budget`, `notifications`;
  `qualify/REPORT.md`). Any pay method (`pay_invoice`, `pay_keysend`, `multi_pay_invoice`, `multi_pay_keysend`) or
  any other method, an empty list or no answer, and SatBooth refuses to start, naming the method.

### Approvals

A payout needs an approval signed by a separate admin system. A person creates the refund there, and it signs
with an Ed25519 key that SatBooth never holds (stdlib `crypto/ed25519`). SatBooth holds only public keys, listed
inline in its configuration; the admin key is rotated by editing the list and restarting. An approval is spent
once it is claimed: after a refund has `failed`, a new refund needs a new approval.

A refund approval is 148 bytes, sent as 296 lowercase hex characters:

| Offset | Size | Field |
|---|---|---|
| 0 | 27 | ASCII `satbooth/refund-approval/v1` |
| 27 | 1 | network: 0 mainnet, 1 testnet, 2 signet, 3 regtest |
| 28 | 8 | key id: first 8 bytes of SHA-256 of the 32-byte Ed25519 public key |
| 36 | 16 | refund id (random, chosen by the admin system) |
| 52 | 16 | order id (the 16 raw bytes of SatBooth's 32-hex order id) |
| 68 | 8 | amount_msat, uint64 big-endian |
| 76 | 8 | expires_at, uint64 big-endian, Unix seconds |
| 84 | 64 | Ed25519 signature over bytes 0..83 |

A void approval is 120 bytes, sent as 240 lowercase hex characters:

| Offset | Size | Field |
|---|---|---|
| 0 | 23 | ASCII `satbooth/refund-void/v1` |
| 23 | 1 | network |
| 24 | 8 | key id |
| 32 | 16 | refund id |
| 48 | 8 | expires_at |
| 56 | 64 | Ed25519 signature over bytes 0..55 |

Both are accepted only if all of these hold: the length is exact and the hex is lowercase; the domain string is
exact; the network is the configured one; the key id matches a configured key; the signature verifies; and
`now < expires_at <= now + 30 days`. A void's refund id must equal the one in the URL. A refund approval also needs
a nonzero refund id and order id, and an amount that is a whole number of sats, above zero, at most 2^53 msat, and
at most `payout.max_msat`. The two domain strings differ, so a refund approval is never a valid void and the
reverse.

The claimant's invoice is **not** part of the approval. The customer adds it later from the merchant's app, where
they are signed in, and each approval can be claimed once, in one history of the store (a second claim of the same
refund id is refused `refund_exists`; a restore of a backup is covered by the procedure below). What this leaves open:
a compromised merchant app can redirect refunds that have already been approved (bounded by the caps below and the
hub's budget), but it can never create or enlarge one. The approval does not
sign the invoice: the approver can't tell whose invoice a string is, so it would add a human step without
closing that gap. A human creates refunds, and a human voids them.

### Restoring a backup while refunds are in use

"Already claimed" is recorded only in the store (`RefundByID`: a refund id that has a row is refused `refund_exists`),
and an approval stays valid until its own `expires_at`, at most 30 days after it is checked (`approval.MaxLifetime`).
A restore puts the store back to the moment of the backup, so a refund claimed after that moment has no row. An
approval signed before the restore, for such a refund, could be claimed again with a new invoice, and the second
payout would be paid inside the approval's amount, `payout.max_msat`, the daily cap and the payout connection's
lifetime budget. The first payout is in the hub, not in the restored store.

The procedure (`docs/backup-and-restore.md`, "Backup and restore"; `deploy/README.md`, "Restore") is one rule: before
the first start after the restore, `payout.approval_keys` is replaced by new public keys only, the `payout` block stays,
and no refund is approved with a new key until the payouts made after the backup are reconciled by hand (a void approval
moves no money and may be signed at any time). An approval whose key is not configured is refused `invalid_approval` at
claim. What the start does to the refunds of the restored store:
- `claimed`: it ends `failed` before anything is sent, `key_revoked`, or `expired_unsent` when its approval has
  expired;
- `sent`: it is looked up at the hub; `settled` or `failed` ends it. `pending` leaves it open (an `ALERT` once an hour
  has passed since the send) and a void is refused (`refund_in_flight`) until the hub settles or fails the payment. No
  record, or `expired`, logs an `ALERT` and leaves it open until a void approval, signed with a new key, fails it with
  the reason `voided` (accepted from 11 minutes after the send). An answer that does not fit the refund (not an
  outgoing payment, another invoice or amount, or a malformed one) logs an `ALERT`, leaves it open and makes a void
  answer `hub_unavailable`: a person investigates. The hub unreachable: it stays open and is looked up again; a void
  answers `hub_unavailable`.

A start without the `payout` block adds nothing to this and is refused while the restored store holds a refund with no
result. The reconciliation is a person's work: SatBooth does not detect that a restore happened. The operator lists
the payments of the payout connection made at or after the backup's `created_at` (in `manifest.json`) and matches each
to the claim that carried its invoice, in the admin system's records (the refund view never carries the invoice).

A pay request sent before the restore can still arrive. The hub does not read a request's expiration (below): only
SatBooth's relay, on the hub host and by that host's clock, stops serving a request past its send time plus
`payout.timeout_seconds` (10 to 600). The reconciliation therefore waits at least 11 minutes after the old SatBooth
stopped or its host went down, whichever is later (when unsure, from the time the operator is sure): the longest
timeout, 10 minutes, plus a minute for the difference between the two hosts' clocks, both kept by NTP and neither
stepped by hand. A payout the hub still shows `pending` counts as paid until the hub shows it `failed` (reconcile again
then; nothing is approved for that order before). Otherwise a request that had not reached the hub, or a payout still
pending there, would leave nothing to subtract, and a replacement refund would pay the same claimant twice. What the
wait does not cover: a hub that was stopped or cut off while it held such a request, or had received it and not yet
recorded it, and resumes later, can still pay it, because the hub accepts a request by its own 6-hour `created_at`
rule alone. Before approving anything for an order with a refund in the hub's list that the restored store does not
know, the operator reconciles again; a payout that appears after a reconciliation is subtracted from that order's later
approvals like any other.

Residuals, stated plainly:
- For an order whose refund was paid after the backup, the restored store and ledger never learn the payout. The
  order's refunded amount stays too low and the refundable check does not stop a refund that repeats it. Nothing
  corrects either by hand; the admin system must subtract the payout from every later approval for that order.
- A refund approved with the new key before the reconciliation is checked only against the restored figures.

A restore takes back more than refunds. An order made after the backup is unknown to the restored store: a payment to
its invoice, address or subaddress is not recorded and not alerted (outputs to an address the store has no row for are
skipped), and the merchant handles it by hand. A payment made after the backup to an invoice, address or subaddress that
the backup holds is found again; one to something issued after the backup is not, even when its order is in the backup
(outputs to an address or subaddress the store has no row for are skipped). A Lightning invoice still open is looked up
and recorded paid. On-chain and Monero a payment that is found again is first seen at the restore: when its order had
expired by then and the restore falls inside the 7-day watch window it is recorded late (`order.late` and an `ALERT`);
past the window only the after-window `ALERT` names its order; before the expiry it counts as on time and `order.paid`
goes out again with the same `id` as before the restore (an event's id is derived from its type and its order), so a
receiver that deduplicates on `id` ignores it. For a late record or an after-window `ALERT` the merchant may already
have had `order.paid`, and checks its own records before crediting or refunding by hand. The addresses and
subaddresses issued after the backup are not issued again while bitcoind's wallet and the Monero wallet keep their
state: the next index comes from them (`getnewaddress`, `create_address`) and must be strictly above the
highest index the store records. A rebuilt wallet has lost that state: for bitcoind the recovery imports the descriptor
with a next index well above anything issued since the backup, and for Monero the operator makes subaddresses in the
account up to such an index, because SatBooth compares only with its own restored store. An address shown to the
customer of an order the restore lost must never be given to a new order: a late payment to it would mark the new order
paid. The backup covers `store.db` and `ledger.db` only.

### Limits

- **Per order, partial refunds allowed.** An order may have several refunds, on the rail of the payment: a Lightning
  payout only refunds Lightning payments, in msat. A new refund is admitted only if the order's Lightning paid msat
  (every paid invoice of the order) minus the amounts of its refunds that are not `failed` is at least the new
  amount.
- **Per refund:** at most `payout.max_msat`.
- **Daily:** the sum of SatBooth's own refund rows claimed on the current UTC day (by `claimed_at`), counting every
  refund that is not `failed`, may not exceed `payout.daily_msat`. The count comes from SatBooth's own rows, not the
  hub's history: the hub's lifetime budget is already the hard cap, and its transaction list is ordered by
  `updated_at`, so a daily total from it would mean paging a hub-wide list.
- **Open refunds:** at most 64 claimed or sent refunds at once.
- A maximum routing fee. NWC can't set one: the hub caps it at max(1 %, 10 sats), and the budget counts amount plus
  that reserve. SatBooth requires budget headroom of the amount plus the larger of 1 % and 10,000 msat before it sends,
  and alerts at `payout.budget_alert_msat`.
- A refund that is `failed`, for any reason, no longer counts against the order's cap or the daily cap.

### The claimant's invoice

SatBooth checks the string's form, and reads only the amount from it: 1 to 2048 bytes; lowercase only; no whitespace
and no `lightning:` prefix; a human-readable part (everything before the last `1`) of `ln`, the network code
(`bc`, `tb`, `tbs` or `bcrt`, equal to the configured network's), and an optional amount; a data part of at least
117 characters of the bech32 set; and a valid BIP-173 checksum (no 90-character limit). It must also:

- carry exactly the approved amount, and be paid exactly that. SatBooth always sends `amount`, and the hub refuses an
  invoice whose own amount differs. SatBooth reads the amount in the human-readable part (digits and an optional
  `m`, `u`, `n` or `p`, BOLT 11's multipliers of a bitcoin, converted to msat) and refuses the claim
  (`invalid_invoice`) when it differs, when it is malformed (a leading zero, zero, a `p` amount that is not a whole
  msat, or more than 2^53 msat), or when there is none. An amountless invoice is refused by default: the pinned hub
  cannot issue one, so paying one cannot be qualified (`dependencies.md` §4). It reads nothing from the data part;
- not be expired (the hub refuses expired ones);
- not pay our own node. The hub would settle that internally rather than refuse it, so SatBooth checks against the
  invoices it issued (the primary check), then with a `lookup_invoice`: if the hub already knows the invoice, of any
  type or state, the refund ends `failed` (`invoice_known`) before anything is sent.

Those hub checks go beyond NIP-47; see `dependencies.md` §4 for the pinned version and its tests.

### Exactly one payout per refund

- SatBooth stores the claimant's invoice string before sending (it decodes no BOLT11, so it keeps the string rather
  than the payment hash), then writes a send marker, then sends `pay_invoice`. **One `pay_invoice` per refund,
  ever:** once the marker exists, only lookups follow. A restart never re-sends.
- On a timeout, SatBooth looks the payment up with `lookup_invoice` instead of retrying, which could pay twice. The
  hub also refuses a second payment of the same invoice while one is pending or settled, but not after the first
  went `failed` (`transactions/transactions_service.go:325-357`).

### State machine

A refund is `claimed`, `sent`, `settled` or `failed`. These are the only transitions:

| From | Event | To |
|---|---|---|
| claimed | approval expired before a send | failed (`expired_unsent`) |
| claimed | its key is no longer configured (re-checked before sending) | failed (`key_revoked`) |
| claimed | the hub knows the invoice (any type or state) | failed (`invoice_known`) |
| claimed | the hub's lookup answers with a row of another invoice | claimed (`ALERT` once; retried as after a failed lookup) |
| claimed | send marker written, `pay_invoice` sent | sent |
| sent | `pay_invoice` returned a relay refusal (`*nwc.RelayRefusedError`) | failed (`not_sent`) |
| sent | lookup: settled, outgoing, same invoice, same amount | settled |
| sent | lookup: failed, outgoing, same invoice, same amount | failed (`failed`) |
| claimed | a signed void (V1a); the ledger holds no pending transfer | failed (`cancelled`); the pending is created and voided |
| claimed | a signed void (V1b); the ledger's pending transfer is neither posted nor voided | failed (`cancelled`); the pending is voided |
| claimed | a signed void (V1c); the ledger's pending transfer is posted | settled (`ledger_paid`); no ledger write |
| sent | a signed void 11 min after the send, the sender done with it (V1d); the ledger's pending transfer is open; a fresh lookup: settled / failed / no record or `expired` | settled (the pending is posted) / failed (`failed`) / failed (`voided`), the pending voided for both |
| sent | a signed void 11 min after the send, the sender done with it (V1e); the ledger holds no pending transfer; a fresh lookup: settled / failed / no record or `expired` | settled (the refund's pending transfer is created, then posted) / failed (`failed`) / failed (`voided`), no ledger write for either |
| sent | lookup: pending | sent (`ALERT` once after 1 hour) |
| sent | lookup: no record or `expired` | sent (`ALERT` once; a person voids) |
| sent | lookup: anomaly (other invoice, amount or type; malformed) or an error | sent (`ALERT` for an anomaly) |
| start | the ledger transfer is resolved but the result row is missing | settled or failed (`recovered`) |

Only a relay refusal counts as never sent: a closed connection or a context error can follow a request that was
already published. Every transition to `failed` also voids the ledger's pending transfer, if one exists. Every
transition runs under one mutex and first re-reads the refund's state; if it changed, the transition does nothing.
Anything outside this table raises an `ALERT` and changes nothing.

**Settled is paid, failed is failed.** A hub-reported `failed` ends the refund at once and frees its amount. The hub
shows `expired` for any unsettled outgoing row past the invoice's expiry, whether it failed or is still pending
(getAlby/hub `nip47/models/transactions.go:28-31`), and a refund the hub has no record of may simply not have arrived.
Neither proves the payment will not happen, so the refund stays `sent` and a person ends it with a signed void.

The one known exception, which operators should know: if the hub is shut down while a payment is in flight, it
writes the row `failed` and can settle it after its restart (getAlby/hub `921003b`,
`transactions/transactions_service.go:452-454` and `:913-920`; `lnclient/ldk/ldk.go`, SendPaymentSync). The
guard is that paying again needs a new approval, signed by a person who can first check the hub.

A void is refused (`refund_in_flight`) while SatBooth's own sender can still publish the refund's request: from the
send marker until its `pay_invoice` call has returned. That mark is kept in memory, which is enough because the data
directory's lock makes the process the only sender, a sender that crashed publishes nothing, and a refund with a
marker is never sent after a restart. It does not reach a request already published.

**The void (rows V1a to V1e).** A person can end any open refund with a signed void, except one the hub itself
reports pending. The void checks, in this order: the approval verifies and names the refund (`invalid_approval`,
`approval_expired`); the refund exists (404); it has no result, and no decided result is waiting for its write
(`invalid_state`); and SatBooth's own sender can no longer publish it (`refund_in_flight`). It then reads the ledger.
A voided pending transfer, a pending both posted and voided, and a posted transfer on a `sent` refund are answered
`invalid_state` with an `ALERT`, and nothing is written. Start-up recovery ends the first (`recovered`) and the third
(settled); for the second it only raises an `ALERT`, and the ledger never produces that state (a pending is posted or
voided once).
A `claimed` refund (V1a to V1c) needs no hub call: the checks, the choice of row and its writes run in one hold of the
transition mutex. The ledger moves before the result row there too: V1a writes the refund's pending transfer and
its void in one ledger batch, applied whole or not at all, so the cancel is decided in the ledger and no crash leaves
an open pending. The send marker is written under the same mutex before the one `pay_invoice`, so a refund without a
marker was never sent from this data directory, and a sender that reads after the void finds the result. A `sent`
refund (V1d, V1e) is refused `refund_in_flight` until 11 minutes after `sent_at`; then the void releases the mutex for
one fresh lookup, takes it again and repeats the checks, and answers `invalid_state` if the refund's state or the
ledger's reading changed. A lookup that answers pending is refused `refund_in_flight`; one that does not match the
refund answers `hub_unavailable` with an `ALERT`; an error answers `hub_unavailable`.

When the store and the ledger disagree, the void follows the record that kept its data, plus the hub's fresh answer,
and never reverses a ledger entry: a posted transfer stays posted. Where the ledger shows the refund paid, a void can
only confirm it paid (V1c). Where the ledger holds no transfer, the hub's fresh answer decides (V1e). A void never
marks paid a refund that no record shows as paid, and the record is the ledger's posted transfer. V1e's settled case
is the only one in which a void creates a ledger transfer, and it is the transfer the refund would have had: byte for
byte the pending transfer the send writes (same id, accounts, amount, order, ledger, code and flag), then its post. If
the ledger refuses the pending, an `ALERT` is raised and nothing is written. If it refuses the post after the pending
was created, an `ALERT` is raised and the pending stays: the refund is then `sent` with an open pending, row V1d's
state, which the resolver's next lookup or a later void ends; nothing is rolled back. Either way the void answers an
internal error and the refund stays `sent`. A result the store refuses waits with its row and is retried with that
row's ledger step and no other; that void answers an internal error. The wait is kept in memory, so after a restart
start-up recovery reads the ledger again: a refund whose pending the void had voided (V1a, V1b, V1d's failures) ends
`recovered` and is never sent, and a V1e failure, which wrote no ledger transfer, is open again as before the void. A
void of a `claimed` refund whose process dies right after the ledger's void of its pending (V1a, V1b) likewise ends
`recovered` at the next start, not `cancelled`. V1a's batch holds the pending first; if the ledger refuses it (the
merchant's account holds less than the refund, the refusal that stops the sender's `prepare` too), nothing is made to
void and the cancel is the result row alone, with no `ALERT`. That is the one cancel a refused result leaves only in
memory: a restart loses it, and the refund is claimed again. The API shows `cancelled` as the reason of a failed
refund; `ledger_paid` is recorded on a settled refund, which, like every settled refund, shows no reason.

Residual (plan 9, the void): **a request already published can be delivered after a void if the clock is stepped
forward and back.** A request the relay still holds (the hub disconnected) is served again once the host clock is
stepped forward past the void wait, a void reads no record and ends the refund `voided`, and the clock is stepped
back before the request's expiration: the relay deletes expired events only when it prunes (on an add or a query),
and its liveness check reads its current clock. The operator's mitigation is NTP on SatBooth's and the relay's hosts
and no manual clock step while a refund is `sent` (`docs/refunds.md`, "Refunds"). This is a known limit, not a
guarantee.

Residual (plan 9, the void): **a request the hub received but has not yet recorded can pay after a void.** A void's
lookup reads no record until the hub commits the payment's pending row. Inside a running hub that gap is short
(getAlby/hub `921003b`): each request runs in its own goroutine (`service/start.go:240`), and before the pending row
commits the only waits are SQLite's 5 s busy timeout on each statement (`db/db.go:63`; SQLite is the hub's default
database, and `deploy/hub` keeps it) and a process-wide mutex that each payment holds only for the transaction that
writes its row (`transactions/transactions_service.go:364-422`). A timeout at the request's first write drops it
(`nip47/event_handler.go:57-72`), and one in the transaction that writes the payment's row answers an error; neither
pays late. The hub's code never reads a request's NIP-40 expiration: it drops only a `pay_invoice` whose `created_at`
is more than 6 h old (`nip47/event_handler.go:352-360`), and it subscribes without `since` on every start and every
re-subscription (`service/start.go:198-205`). The bound on a stored request is therefore SatBooth's relay alone, which
re-checks a request's expiration as it writes each frame (`relay/conn.go:165`); the request expires at `sent_at` plus
`timeout_seconds`, at least a minute before the void wait ends. What is left: a hub process frozen for minutes (a
paused VM, a sleeping host) with the request already in its socket but unread, which wakes while a void's lookup is
in flight. Such a payment leaves the refund `failed`, and the merchant reconciles it by hand. This is a known limit,
not a guarantee.

### API surface

`POST /v1/refunds` and `POST /v1/refunds/{id}/void` need the `refunds:write` capability, and
`GET /v1/refunds/{id}` needs `refunds:read`. Bodies have exactly the listed keys. Every response is built from an
allowlist of fields: id, order id, amount, state, a fixed reason on `failed`, and the claim and resolution times.
It never carries the invoice, a payment hash or a preimage. Without a `payout` block, every refund route answers 404
`refunds_disabled`. The merchant's app polls the refund; there are no refund webhooks.

## 5. Listeners and HTTP

- Internal services (hub, relay, chain watchers) are never public. They sit on localhost, a private
  network or an authenticated tunnel. As built, SatBooth reaches bitcoind over the private link with its own RPC
  user (`rpcauth`), confined by `rpcwhitelist` to twelve methods; its client refuses any other method before sending,
  follows no redirect, uses no proxy from the environment and reads at most 8 MiB of an answer. Since plan 8 (as
  built) it reaches wallet-rpc the same way: over the private link, JSON-RPC 2.0 to `/json_rpc` with HTTP
  Digest, only the eight methods of §0 (any other is refused before sending), no redirect followed, no proxy from the
  environment, a timeout, and at most 8 MiB of an answer. Since plan 7, with the `rates` block, the public host also
  calls the rate providers on the public internet (`api.kraken.com`, `api.coinbase.com`, `api.yadio.io` and the
  operator's custom URL; since plan 8 also `api-pub.bitfinex.com`): fixed URLs, `GET` only, HTTPS only
  (TLS 1.2 or later), no redirect followed, no proxy from the environment, a 10 s timeout, at most 256 KiB of an
  answer, one request per provider per `fetch_seconds` whatever the traffic, and nothing from an answer in a log, an
  error or a response (§10, threat 3).
- The per-host bundles (plan 10, as built; `deploy/README.md`, `design.md` §3 "Operations, as built (plan 10)").
  The hub host publishes on its private-link address only the three things SatBooth calls: the relay's client listener (7448),
  bitcoind's RPC (8332) and, with the `monero` profile, wallet-rpc (18083). The hub's admin UI and API (8080) is
  published on a separate address, loopback in the example, reached through an SSH tunnel, so a compromised public
  host cannot reach it over the link. The public host publishes only the checkout page. Every published address is a
  required value, so an empty one is an error, never every interface (`scripts/check.sh`). Inside the hub host,
  monerod and wallet-rpc have a network of their own: no other container can call monerod's RPC, which has no login
  and, unrestricted, could be pointed at a chain of someone else's making (`set_bootstrap_daemon`) that wallet-rpc
  would then report as payments; monerod also runs with `--restricted-rpc`. Fulcrum is on an internal network with
  only bitcoind and the hub's namespace, with no route out. The example `bitcoin.conf` confines Fulcrum's bitcoind
  user by `rpcwhitelist` to the 15 calls Fulcrum 2.1.4 makes. Accepted residuals:
  - **bitcoind and Fulcrum can reach the hub's API on 8080.** The hub's HTTP listener binds every address of its
    namespace and has no setting for the address (getAlby/hub v1.24.6 `cmd/http/main.go:59`), and a published port
    reaches the container's address, not its loopback, so the hub's listener cannot be narrowed to the tunnel without
    a forwarder or firewall rules inside the namespace. So a compromised bitcoind (on the `hub` network) or Fulcrum (a
    parser bug fed by chain data, on the `electrum` network) can connect to the API. It still needs the unlock
    password or a token, and the hub limits unlock attempts with one global bucket of 1 a second, burst 2
    (`http/http_service.go:99-113`). A hub that has not been set up yet belongs to whoever opens its setup first. The
    bundle's setup order narrows that window: the first `up` starts only `volume-init`, `netns`, bitcoind and the hub,
    and the operator sets the unlock password through the tunnel before Fulcrum, the relay or the rest start, and
    before the public host is connected to the private link (`deploy/README.md`, "The hub host"). bitcoind is already
    running in that window, so the window is bounded by how soon the operator sets the password, not closed.
  - **The hub's own bitcoind RPC user.** The example `bitcoin.conf` confines it by `rpcwhitelist` to the eleven
    calls the hub's chain source makes (`docs/dependencies.md` §8): reads of the chain, the mempool and fee
    estimates, and `sendrawtransaction`. Whoever holds that password (in `hub.env`, 0600, on the hub host) or
    controls the hub can still broadcast transactions and read what the node knows, but not stop the node, load a
    wallet or touch SatBooth's watch-only wallet. `rpcwhitelistdefault=0` keeps the cookie of `bitcoin-cli` inside
    the bitcoind container unrestricted. The hub host already holds the Lightning funds, so this is part of a
    hub-host compromise rather than a wider bound for a SatBooth compromise.
- Strict server limits:
  - timeouts on headers, reads, writes and idle connections;
  - header and body size caps;
  - a concurrency cap.

  As built, on both listeners: 5 s for headers, 10 s to read, 15 s to write, 60 s idle; headers at most 8 KiB and
  bodies at most 16 KiB; 256 requests in flight.
- Short timeouts on every outbound call.
- One key per capability, compared in constant time, and rotatable without a restart. As built, a key holds
  `orders:write` or `orders:read`, the file holds only SHA-256 hashes, and SIGHUP reloads it (a refused file leaves
  the old keys in force).
- Fixed routes only: SatBooth builds every upstream request itself, with no pass-through of paths or
  parameters.

## 6. Supply chain

- Minimal dependencies, pinned and reviewed.
- Vulnerability scanning on each toolchain release.
- Reproducible builds, and container images pinned by digest.
- As built, the release images are `FROM scratch`, built from the digest-pinned builder
  (`golang:1.27.4-trixie`) with vendored modules and no module download (`GOFLAGS=-mod=vendor GOPROXY=off
  GOTOOLCHAIN=local CGO_ENABLED=0`, `-trimpath -ldflags=-buildid=`). The `satbooth` image holds two files, `/satbooth`
  and `/etc/ssl/certs/ca-certificates.crt`; the `satbooth-relay` image holds one, `/satbooth-relay`; the
  `satbooth-tools` image holds `/satbooth-setup` and `/satbooth-pair`. `scripts/image-check.sh` refuses any other file,
  in CI, and `.dockerignore` keeps the build context to the Go sources, `vendor/`, `go.mod`, `go.sum` and the three
  files `checkout` embeds (`page.html`, `style.css`, `logo.svg`). They run as 65532:65532 with a read-only root
  filesystem, all capabilities dropped and `no-new-privileges`. On the hub host, bitcoind, Fulcrum, monerod and
  wallet-rpc are built from the official release tarballs, checked against pinned SHA-256 hashes, on a digest-pinned
  `debian:13-slim`, and Alby Hub is its release image pulled by digest; SatBooth's own images never contain them
  (`dependencies.md` §8).

**Release images.** A release publishes the three SatBooth images to GHCR. The build is reproducible:
`scripts/release-build.sh` builds for `linux/amd64` and `linux/arm64` with a BuildKit pinned by digest, fixed layer
compression and file times, and the same sources give the same digests on any machine. The deploy bundles and
`deploy/setup.sh` name each image by `tag@digest`, so what a host runs is decided by a file in git, not by the registry.

What an operator verifies:

- The tag. The maintainer signs the annotated tag `v<major>.<minor>.<patch>[-rc.<n>]` with the release signing key. The
  operator imports that key, compares its full fingerprint with the one in `SECURITY.md` ("Release signing key"), and
  runs `git tag -v <tag>`. The tag signature is the only check of who authorised a release.
- The pins in that tag's tree (`scripts/release-pins.sh show`): they are what the bundles pull.
- Optionally, the attestation of each image: `gh attestation verify` with `--signer-workflow
  SatBooth/SatBooth/.github/workflows/release.yml`, `--source-ref refs/tags/<tag>` and `--deny-self-hosted-runners`
  accepts only a build by the release workflow, run from that tag on a GitHub-hosted runner.
- Optionally, a rebuild, with the output directory outside the checkout: `scripts/release-build.sh "$out"` and
  `scripts/release-pins.sh check "$out/digests.txt"` must report every digest as matching.

What the release workflow can do. A tag push runs `.github/workflows/release.yml` from the tagged commit. Two builders,
amd64 and arm64, must produce identical digests, and those must equal the pins of the tagged tree. Only then, and only
after the `release` environment is approved, does the publish job push the images to the three GHCR packages, attest
them and (in a later job) create a draft release. The workflow checks that the tag is annotated, named like a release
and at the run's commit; it does not verify the tag's signature, because a check written in the tagged tree would stop
no one who controls that tree. The controls that decide who can make the workflow publish are two repository
settings, which `docs/releasing.md` ("Before the first release") sets up and reads back. They are required
configuration, not something the workflow enforces: the `release` environment needs the maintainer's approval, accepts
deployments from `v*` tags only and cannot be bypassed by administrators; a ruleset on `refs/tags/v*` forbids update
and deletion without bypass and restricts creation to the repository-admin role. A workflow that names an environment
that does not exist gets one created without protection, so the settings must exist before the first release. The
publish job pushes the pinned digests and tags them with the version; it fails when a registry tag already holds
another digest.

What a compromised run or a stolen token could still do. Any workflow that someone with write access to the repository
pushes, on any branch, can request `packages: write` and push to the three packages without the environment. A
compromised publish job could do the same. Either could push other digests or move a registry tag. Operators who use
the tagged bundles pull by digest and are unaffected: the digest in the signed tag's tree names the content, whatever a
registry tag says. A moved tag shows as a tag whose digest differs from the tagged pin (`scripts/release-smoke.sh
<tag>` checks this, and refuses it). An operator who pulls by a registry tag takes this risk; the bundles do not. An
image pushed by another workflow has no attestation from the release workflow, and fails the `gh attestation verify`
command above.

A stolen credential of a repository administrator can create a `v*` tag and approve the `release` environment. The run
then builds the tagged tree, attests it and passes the `smoke` job: the attestation is valid, because it proves where an
image was built, not who authorised it. The signature of the tag is the only check of the latter, and an attacker
without the release signing key cannot make one that verifies. Anyone with write access can also create or edit
releases through a workflow, so the text of a release (its image refs and commands) is not authoritative. The signed
tag's tree is.

What is trusted. A rebuild checks the hosted runners and the release environment: a runner that built something else
produces digests that differ from the operator's. It does not check the builder images. The pinned `moby/buildkit` and
`golang` images are trusted alike by the release build and by every rebuild, which run the same pinned builders; a
compromise of either, at the pinned digest, would give the same wrong digests in both. The pins are by digest and are
reviewed when they change. The draft release is published by the maintainer by hand, only after the `smoke` job (both
architectures) and an anonymous pull of the three packages have passed (`docs/releasing.md`).

## 7. Public DDoS

The only public surface is behind a CDN/WAF: DDoS protection, challenges, rate limits per IP/ASN on the
invoice endpoints.

## 8. Privacy

- Invoice descriptions and labels carry an opaque order id, never a user id, npub or email. As built, the
  description is `SatBooth order <id>`.
- On-chain addresses get no label. The API returns an order's address (`onchain.address`): it is the merchant's own,
  derived from their descriptor, and the merchant's wallet sees the same payments. The checkout page shows it only
  to the holder of the order's token, and no longer after the expiry.
- Logs never contain keys, preimages, descriptors, addresses or bitcoind's error texts (they can echo a key);
  a refused descriptor is never logged.
- The rate providers (plan 7, with the `rates` block) see SatBooth's address, the header `User-Agent: SatBooth` and
  the fetch interval, and the asset and currency codes asked; never an order, an amount or a customer. The fetcher
  runs on its own schedule, so the requests do not follow the order traffic. The custom URL, which may hold a token
  (then in `rates.custom_url_file`, a 0600 file), is never logged.
- Monero (plan 8, as built): subaddresses get no label beyond a fixed `satbooth`, and they are the
  merchant's own: the merchant's wallet sees the same payments. The wallet's view key reveals every incoming payment
  of the whole wallet, all accounts, so a leaked view key (§0) exposes every payment to it, not only the shop's; hence
  the advice in `docs/monero.md` that the operator create a fresh wallet used only for shop income. The checkout page's
  Monero link is `monero:<subaddress>?tx_amount=<xmr>`: the address and the amount only, no `recipient_name` and no
  `tx_description`, so the payer's wallet stores nothing about the shop beyond the address. As for bitcoind, logs never
  contain an address or wallet-rpc's answers and error texts: a log line carries a method name, an error code, an order
  id, a step number or a count.

## 9. Operations

Alerts on:
- the invoice-creation rate;
- limit refusals, by class;
- the number and total of payouts per day;
- NWC timeouts;
- chain-watcher lag (blocks behind tip);
- hub channel balance above the sweep threshold.

Plan 10 (as built): an optional `alerts` block queues every `ALERT:` line and posts it to an operator
webhook with the existing `SatBooth-Signature` header, so the operator's monitoring can raise the alert. The payload
holds the message, an allowlisted subset of the line's attributes, the number of attributes dropped and a repeat count
(alerts coalesce by hour; at most 1,000 are stored per UTC day, counted from the store; a full in-memory queue of 256
entries drops the alert and logs `alert queue overflow`), never a secret (§0). Delivery retries as the merchant
webhooks do and gives up after 24 hours with a line that is not itself an alert; an alert already delivered is not
sent again when its repeat count grows. A full queue or a failing store never blocks or loses the log line, which is
always written. Without the block, the service only logs, as before. Sending a received alert to a person, and the
thresholds of the other alerts above, are the operator's monitoring. The lines to alert on are the ones that start
with `ALERT` (a stale invoice, a settlement below the invoice amount, an invoice closed at its 24 hour deadline, a
second paid invoice for one order, a payment record with other details, a bad wallet answer, a failed store write,
an order amount the current limits refuse, a late payment after the order expired, `ALERT: webhook delivery abandoned after 24 hours`,
and, since plan 7, `ALERT: a webhook event is held back: its order's stored quote is malformed`, once per event per
run; `docs/webhooks.md` lists them). Of the rest of the list above: refusals by limit are
not logged, except that amount refusal; NWC failures are logged as the lines `make_invoice failed` and `invoice lookup failed; the invoice stays open` (at the
info level, without telling a timeout from another error); the hub's balance is not logged. Chain lag is logged
since plan 6: `ALERT: bitcoind is in initial block download or behind its headers` on every pass while bitcoind is
syncing or more than 3 blocks behind its headers, and `ALERT: bitcoind call failed` on every pass bitcoind does not
answer; the checkout page reads "Checking payment…", and the API's `onchain.stale` is true, when no pass has
succeeded for 3 poll intervals plus the last good pass's duration, and (since plan 6.1) for one order alone when the
last pass could not bring it up to date (its settle failed, or a call for one of its payments did). SatBooth's own
lag behind bitcoind's tip is not measured. The on-chain `ALERT` lines are listed in `docs/rules-onchain.md` ("Rules for
on-chain payments"). Since plan 7, `ALERT: fiat pricing is unavailable for a currency` (with `asset`, `currency`,
`reason` `too_few` or `disagree`, the `providers` and, for a disagreement, the `low` and `high` ones, names only) marks
a currency that cannot be priced, logged when its state changes; `fiat pricing is available for a currency` marks the
recovery, and each failed provider request logs `rate fetch failed` (with the provider, an error class and the HTTP
status; a warning for the first failure of a provider's run of failures, debug for the rest of the run).
`docs/rules-fiat.md` lists them ("Rules for fiat pricing").

Plan 10 (as built): the pair stamp. The store and the ledger each hold a random pair id, a generation that every
start advances, and a random nonce that every advance draws afresh; SatBooth refuses to start on two files that are
not one pair at one moment (`design.md` §3, "Operations, as built (plan 10)"). It covers an operator's mistake in a
restore: a store restored without its ledger or the reverse, a file of another installation, a lost file recreated
empty, and a mix of two timelines (after a restore, backups made on the timeline given up reach the same generations
with other nonces). Without it SatBooth would serve on a store and a ledger that disagree about which orders are paid
and which refunds exist. The refusal is a fixed message with no id, nonce or path, and it has no override. It does not
compare content: a hot copy of one file is not a backup, which is why `satbooth backup` is offline. A start that
finishes an interrupted advance advances once more with a fresh nonce, so two copies of one crashed data directory end
on different nonces after their first start, and mixing their files later is refused. Accepted residual: a stale
ledger file that still holds its pending mark, copied back beside the store it was marked for before either was
started again, is not told apart from the original. Only a pair made by `satbooth backup`, which refuses an
interrupted state, is supported for a restore.

Plan 8 (written before the code, **as built**) adds the Monero lines, each a fixed text with an order id, a count, a
step number, a one-word reason or a method and an error code, never an address, a key or wallet-rpc's text.
`docs/rules-monero.md` lists them ("Rules for Monero payments"). At startup and when a subaddress is issued:
- `ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it catches up` (at startup,
  and once per pass while the wallet is below the cursor; the scan never moves the cursor down);
- `ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record`, with the count;
- `ALERT: a new subaddress failed its checks; not shown`, with the step number, never the address;
- `ALERT: the wallet-rpc check failed; subaddresses are not issued` (a re-check that fails while the wallet is behind
  on subaddresses; the watcher goes on);
- `ALERT: Monero watching is paused: the wallet failed its startup check` (ruling 8-T7c), with a one-word `reason`, once
  per pass while a re-check fails for any other reason than subaddresses (the wallet replaced, the login refused,
  wallet-rpc unreachable). A wallet replaced while SatBooth runs is caught at the next pass or press: every pass that
  does not re-run the check and every press that needs a new subaddress (before `create_address`) first compares the
  account's base address with the one the last passed check kept, one `get_address` call each; the press compares it
  again in the `get_address` answer that cross-checks the new subaddress (ruling 8-A3), so a wallet swapped between the
  first read and `create_address`, which answers both later calls consistently, is caught by its base address. On a
  difference it pauses with `reason=address` before it records anything, and the press answers busy, so no subaddress of
  another wallet is shown to a customer. The pass then records, marks and settles nothing: a wrong wallet answers "not
  found" for every transaction, which would read as every tracked payment gone and send false `order.reverted`. The
  cost: Monero payments wait until the wallet is fixed, and one first seen after the expiry because of the pause reads
  `late`. The startup check itself is fatal (`startup failed`, reason `monero`, with the check's fixed text as the
  `cause`).

In the watcher's passes:
- `ALERT: wallet-rpc call failed` (with the method and the code; one line per pass that fails);
- `ALERT: a Monero payment arrived after the subaddress's watch window`;
- `ALERT: a Monero payment is locked by its sender beyond the normal lock; it is treated as late` (once, when the
  payment is first discovered);
- `ALERT: a Monero payment's inputs were seen double spent; the block decides` (once per payment per process, on a
  tier that needs a block, at most 16 lines a pass), then `ALERT: more Monero payments' inputs were seen double spent;
  the block decides` with the count of the rest (bounded as the oversized line below);
- `ALERT: wallet-rpc answered with a bad transfer` (once per payment per process, at most 16 lines a pass), then `ALERT:
  wallet-rpc answered with more bad transfers` with the count of the rest;
- `the wallet changed while the pass read it; nothing from the pass is written, the next pass reads the wallet again`
  (a WARN, ruling 8-A6; with `reason` `height`, `address` or `generation`), and at the third such pass in a row, once
  until a pass is written, `ALERT: the wallet changed during several passes in a row; nothing was written from them:
  is wallet-rpc's wallet or monerod changing?`;
- `ALERT: wallet-rpc listed entries SatBooth could not read; reconcile the wallet's transfers from from_height to
  to_height, and its pool, by hand` (ruling 8-A4; with the count, `from_height` and `to_height`, once per pass while it
  lasts): an entry that fails the shape checks or repeats a key may be a payment to an issued subaddress, and the scan
  position still advances, so the next listings may start above it; the line names the listing's range for a person to
  reconcile from the merchant's wallet. Entries for addresses SatBooth never issued are skipped without it (ruling
  8-T10a-2), and so is a coinbase, type `block`;
- `ALERT: a Monero payment above 2^53-1 piconero to an issued subaddress is not recorded; reconcile it by hand` (ruling
  8-A4; with the order id, once per payment per process, at most 16 lines a pass): the amount is above the bound of
  every amount SatBooth stores (2^53 − 1 piconero, about 9,007 XMR), so the payment is never recorded;
- `ALERT: more Monero payments above 2^53-1 piconero to issued subaddresses are not recorded; reconcile them by hand`
  (ruling R8; with the count): the payments of one pass past those 16 lines, so a corrupt listing cannot flood the
  log; a payment counted here is not logged again (past 4,096 payments in one process, the new ones are counted
  here once per pass);
- `ALERT: the wallet's height has not moved for 30 minutes; is monerod running and synced?` (then at most once per 30
  minutes while it lasts: a wallet that lost its daemon keeps answering);
- `ALERT: unused Monero subaddresses exceed unused_alert; raise the merchant wallet's subaddress lookahead` (with the
  count; once a day while it lasts);
- `ALERT: a Monero order left its watch window without a final event; check it by hand` (also for an order held by a
  stale or pending on-chain rail at its window's end, rulings 8-T7d and 8-T7e);
- `ALERT: a Monero payment left its watch window without one successful observation; reconcile it from the wallet`
  (with the order id, also when the order has its final event; once per payment per process, at most 16
  lines a pass, then `ALERT: more Monero payments left their watch window without one successful observation;
  reconcile them from the wallet` with the count): a late payment whose call failed in every pass of its window (no
  mark) is no longer polled once the window closes, and nothing records it, so a person reconciles it from the
  wallet. An on-time payment is not listed: it stays polled past the window;
- `ALERT: an on-time Monero payment past its watch window left the wallet; SatBooth stops polling it and will notice
  it again only if a scan lists it mined` (the twin of the on-chain line for bitcoind's mempool; not when the order
  reads paid by its events);
- `ALERT: a Monero payment was reverted by a reorg or a double spend`;
- `ALERT: a reverted Monero payment confirmed again`;
- `ALERT: a late Monero payment arrived after the order's expiry` (a locked payment is late too).

The rail-generic lines of the on-chain rail are logged for Monero orders too, with the same texts: `ALERT: an order was
overpaid; refund the excess`, `ALERT: an order was paid on both rails` (once per rail per order per process, so two
lines when both watched rails record), `ALERT: an order was underpaid at expiry`, the window-end text (`ALERT: an order
was underpaid at the end of its watch window; its payment is not yet confirmed enough and stays tracked`), the
paid-after-final text (`ALERT: an order was paid after its order.expired or order.underpaid event; the payment is
recorded and the order reads paid`) and `ALERT: a payment record exists with different details`. Plan 8 also adds, on
both watched rails, `ALERT: an order reverted by a reorg or a double spend was paid again on Lightning`: the paid-again
line names the rail whose money makes the reverted order paid again (the rail's own, the other watched rail's, or
Lightning's). Without the `ALERT` prefix, a payment whose call fails logs `wallet-rpc call failed for a payment`, and
`scan failed`, `track failed`, `settle failed` and `Monero status could not be read` mark store and read failures (the
`at` of `scan failed`, `track failed` and `settle failed` names the store call that failed, and a stop of the service is
not logged as a failure); at
start `wallet-rpc version` logs the version (info). The API's `monero.stale` follows the on-chain rule (3 poll intervals
plus the last pass's duration, and per order), so it also turns true while watching is paused. Monero's lag behind the
chain is wallet-rpc's 20 s auto-refresh plus `poll_seconds` (*observed*: up to 20.6 s from a block to `paid` at
`poll_seconds` 2, `qualify/REPORT.md`); SatBooth's own lag behind monerod is not measured.

### The setup command (plan 12)

`satbooth-setup` (`design.md` §3, "The setup command, as built (plan 12)") writes the configuration and generates the
secrets of a host, so a mistake in it is a mistake in every file the operator then trusts. These are the rules it keeps.

**Where every secret is generated and read.** Each secret is generated on the host that reads it, by the setup run on
that host, from `crypto/rand`, and is written to the file below with mode 0600. The hub's unlock password and the
approval private keys are never seen by setup: the operator sets the first in the hub's page, and the second lives on
neither host (`satbooth-approve` is not in the tools image; setup takes approval public keys only).

| Host | Secret | Written to | Read by |
|---|---|---|---|
| public | NWC client keys (the invoice connection; the payout connection with refunds) | `satbooth.json`'s `nwc.client_key_file` and `payout.client_key_file` in `SATBOOTH_ETC` | SatBooth (65532) |
| public | SatBooth's bitcoind password (on-chain) | `onchain.rpc_password_file` in `SATBOOTH_ETC`; only its `rpcauth` line crosses | SatBooth (65532) |
| public | the API keys | the plain keys in the setup directory; their SHA-256 hashes in `api_keys_file` | the merchant's app holds the keys; SatBooth reads the hashes |
| public | the webhook secrets (one per endpoint since plan 13) and, if configured, the alert secret | `webhooks[].secret_file` and the `alerts` block's secret file in `SATBOOTH_ETC` | SatBooth (65532) |
| hub | the hub's bitcoind password | `hub.env` in `HUB_CONFIG_DIR`; its `rpcauth` line in `bitcoin.conf` | the hub (10003); bitcoind (10001) reads the hash |
| hub | Fulcrum's bitcoind password | `fulcrum.conf`; its `rpcauth` line in `bitcoin.conf` | Fulcrum (10004); bitcoind reads the hash |
| hub | the wallet-rpc login password (Monero) | `wallet-rpc/rpc-login`, and the copy the operator carries | wallet-rpc (10002); SatBooth through the copy |
| hub | the view-only wallet's password (Monero) | `wallet-rpc/wallet.pass` | wallet-rpc (10002) |

The files, their owners and their modes are those of `deploy/README.md`, "The bind-mount rule" and "The hub host"; setup
writes the files an operator would otherwise write by hand and gives no file more access than a by-hand setup does.
Setup creates every file with mode 0600 whatever the umask, and every directory with 0700; the bundle's `volume-init`
then gives each file to the user that reads it. A symbolic link at a target is refused.

**What crosses hosts, and what a swapped value can do.** Only public values cross, in two handoff files. The network
and every address are typed on each host, and no stage writes a network or an address that it read from a handoff:
`public-to-hub.json` carries the public host's copies only so that stage 2 can compare them with the hub host's own
answers and refuse when they differ. The file `public-to-hub.json` has nine fields, and a value swapped on the way does
this:
- `version`: another version is refused by the parser.
- `network`, `hub_private_addr` and `public_private_addr` are compared only: a swap makes stage 2 refuse and write
  nothing.
- `invoice_client` and `payout_client` are the NWC client public keys. A swapped key gives a relay pair that never
  works: the hub approved the key of the link that stage 1 printed, and the relay is configured for the swapped one.
  A `payout_client` added or removed makes stage 3 refuse, since the payout Wallet Public Key it requires no longer
  matches.
- `satbooth_rpcauth` is the `rpcauth` line of SatBooth's bitcoind user. A swapped line gives the attacker's password
  SatBooth's whitelisted bitcoind user. With network reach to bitcoind's RPC (which `rpcallowip` limits to the
  bundle's subnets and the public host's address) that user can read the wallet's descriptor and its payment history
  (`listdescriptors`, `listsinceblock`) and draw addresses (`getnewaddress`), the calls of §0. It cannot spend, since
  the wallet holds no keys, and it cannot make SatBooth see a payment. SatBooth's own password no longer matches the
  line, so SatBooth refuses to start when its on-chain startup check fails. A line added where stage 1 made none (a
  Lightning-only setup) fails nothing, because SatBooth has no on-chain rail to notice: stage 2 writes a `satbooth`
  user with its whitelist, and `rpcallowip` for the public host, into `bitcoin.conf`. That user can make the
  whitelisted calls from the public host's address only, on a bitcoind that has no wallet for it: no descriptor, no
  history and no keys. It needs the file and the digest swapped together.
- `monero` and `monero_rpc_user` decide whether the Monero files are written and which user the login names. A swap
  switched off leaves the Monero rail without its files, and a different login user does not match wallet-rpc's, so
  SatBooth refuses to start; switched on, the hub host gets Monero files that nothing uses (the operator starts the
  Monero profile themselves), and stage 4 then refuses a carried file. None of it moves money.

The file `hub-to-public.json` has three fields: `version`, and the hub's invoice and payout Wallet Public Keys (the
payout key only with refunds; one added or removed makes stage 4 refuse). A swapped Wallet Public Key makes NWC fail
closed: SatBooth's requests are encrypted to a key the hub does not hold, so nothing decrypts, and the relay forwards
only for its configured pairs (only its loopback hub listener can publish as a wallet).

A handoff that is truncated, malformed, of the other kind or of another version is refused by its parser, with one
fixed message per reason. One that was edited within the rules parses, and only the digest catches it. The writing
stage prints the SHA-256 digest of the file's bytes; the file holds no digest of itself, since whoever swaps the file
would recompute it. The reading stage takes the digest from the operator and, before it writes anything, refuses a
file whose digest differs. So the command makes the comparison, not the operator's eye.

**What a handoff cannot decide.** A handoff only fills in what the reading stage has no other source for: the client
public keys, the `rpcauth` line, the Wallet Public Keys, whether the Monero files are written and the Monero
login's user name. It never decides the
network, an address, where money is received or what is trusted as a payer. The pairing links are printed at stage 1,
where the client keys are made: a link built from a key read out of a handoff could get a payout connection approved
for a key the operator never made, which is a payout connection for an attacker. The network is never taken from a
handoff because a swapped network would put the hub on a chain whose coins are worthless, and a payment of those coins
reads as paid: the setup command writes `nwc.network` from the public host's own answer, and SatBooth refuses every
invoice of another network or amount (§3); with the on-chain rail, `onchain/check.go` refuses a wrong chain. A hub address is never taken from a handoff because a swapped
one would publish the relay's, bitcoind's and, with Monero, wallet-rpc's ports on another interface, where only
the relay's NIP-42 AUTH (against its `client_relay_url`) and pair policy, wallet-rpc's login (§0) and bitcoind's
`rpcallowip` would protect them. Stage 4 builds the relay's, bitcoind's and
wallet-rpc's URLs from the hub address the operator typed at stage 1, never from a handoff: a swapped URL would point
a rail at a node that reports payments, and a node the attacker runs can report a payment that was never made, which
marks an order paid (§3).

**The one secret that crosses.** monero-wallet-rpc's login is HTTP Digest, so both hosts need its password in plain
text: the hub host's wallet-rpc to check it and SatBooth to answer the challenge. Stage 2 writes it on the hub host
and a second copy in the setup directory (`setup/carry/wallet-rpc.pass`, 0600); the operator copies that file to the
public host over a channel they trust, and stage 4 reads it there. It is not in either handoff, and no digest covers
it: a swapped copy only makes SatBooth's login fail. Whoever holds the login and can reach wallet-rpc's port can point
the wallet at a daemon they run (`set_daemon`) and so fake Monero payments, and can read the view key (§0).

**Where the plain values live.** The plain API keys (`setup/api-keys/<id>.key`), the carry file and the handoffs live
in the host's setup directory, `<dir>/setup/`, which no service mounts: no container of either bundle can read a plain
API key, and the files SatBooth does read hold only the hashes. Setup names a key's file and never prints the key.
`setup/state.json` keeps the operator's answers, the descriptor and the Monero address included (private data, in a 0600
file that no service mounts), and so does the answers file, which is the operator's own and theirs to delete.

**No network, nothing executed.** The wrapper runs the tools image with `--network none`. Setup opens no network
connection, runs no other program and prints commands for the operator to run instead. It never prints a secret, a
descriptor or a Monero address; the descriptor and the address come from the answers file or a prompt, never argv.
A descriptor with a private key, a seed, an xprv or a Monero spend key is refused with an error (CLAUDE.md, "Nothing to
sweep"). No configuration file, secret or handoff is overwritten: a stage refuses if any file it would write exists,
so no run can replace a key or a password that a service already uses. Setup's own bookkeeping differs: `state.json`
is replaced by a rename, and the journal and the stage's temporary directory are removed when a stage ends. The one
deletion is `-discard-incomplete`: it removes the files whose content is the unfinished run's (the journal holds the
SHA-256 of the bytes each file was to get, so a file made by hand under a journaled name is kept) and the empty
directories that journal of that stage names, restores the previous `state.json`, and refuses a journal it cannot match
to that stage.

**Residual risks.**
- An operator who copies the digest over the same channel as the file gets no protection from it: an attacker who
  controls that channel can replace both. The digest guards against a corrupted copy and against a channel that
  carries only one of the two.
- A workstation or copy channel that reads a handoff learns the hosts' private addresses and public keys. Neither is a
  secret that moves money, but the addresses say where the hub is on the private link.
- The wallet-rpc login is the only protection of wallet-rpc on the private link: the bundle has no address allowlist
  for it, as it has for bitcoind. Whoever holds the carried password and can reach the port can make the Monero rail
  report payments and read the view key. The carried copy stays in `setup/carry/` on the hub host until the operator
  deletes it (stage 3 tells them to), and on the copy channel until the operator deletes it there (stage 4 tells them
  to).
- A compromised hub host can give wrong Wallet Public Keys, so NWC fails or talks to a node it controls. It already
  holds the funds and the node that SatBooth trusts for Lightning payments, so setup adds no power to it.
- Setup trusts the machine it runs on: a host that is already compromised sees every secret as it is generated. Setup
  does not shorten that window; it avoids a workstation that would otherwise hold every secret.

## 10. Rate sources (plan 7, as built)

Written before the code (plan 7, decision 6): a fiat price is converted into per-rail amounts from exchange rates, so
the rate is a money input, and the rate providers are the public host's first outbound destinations on the public
internet besides the relay and the webhook URL (URLs since plan 13, §11; bitcoind is reached over the private
link). **As built** (plan 7):
every mitigation below exists in the code, with the keys, defaults and bounds as written (`rates`, the `rates` block
in `service`, `store`'s `order_quotes`); the paragraphs marked "As built" say how. Vendor claims are marked *source* (the vendor's
documentation, with its URL), *observed* (an unauthenticated request on 2026-11-05) or *inferred* (a reading of
those, not stated by the vendor). Plan 8 (written before the code, as built) adds the XMR rate and Bitfinex as a
fourth provider, under the same rules: items 1, 3 and 7 name them. As built, a pair that fewer than `min_sources` of
the configured providers can ever quote (the custom API counting as yes) is *unreachable*: it is not fetched, raises
no `ALERT`, and a priced order naming `monero` in that currency is a permanent 400 `invalid_currency` (ruling 8-A), so
no client retries it forever; 503 `rate_unavailable` stays for an outage of a reachable pair.

1. **A lying or compromised provider.** A rate is fiat per unit of the asset (bitcoin or monero), so a rate too high
   prices the order too low (the merchant ships goods for less), and a rate too low prices it too high (the sale is
   lost). Mitigation: the median of
   at least `min_sources` providers (default 2), and the spread bound (`max_spread_percent`, default 2%: when the
   fresh rates differ by more, no fiat order is priced), both configurable; the amount bounds of the rails (`limits`:
   `min_msat` and `max_msat` on Lightning; on-chain only `store.MaxSat`, 21 million bitcoin; Monero (plan 8, as built)
   only `store.MaxPiconero`, 2^53 − 1 piconero, about 9,007 XMR, refused with `invalid_monero_piconero`). Residual: a majority of
   the enabled providers agreeing on a wrong rate; inside the bound a liar moves the price undetected, by up to half
   of `max_spread_percent` when only two rates are fresh (the median of two is their mean); one liar among three, or
   one of two, outside the bound blocks fiat pricing (fail closed) until it is removed from the configuration, and
   the `ALERT` names the providers (the sources, and the lowest and highest); explicit orders still work. For
   parallel-market currencies such as ARS and VES, the providers can differ by more than 2% (Yadio quotes the
   parallel rate, the exchanges an official or derived one: *inferred*), so those currencies may never price. The
   rails' bounds catch only gross errors, and on-chain almost none. As built: the median of an even count is the
   lower mean of the two middle rates; the spread rule is exact on integers, `(max − min) × 10000 > max_spread_bps ×
   min`; a converted amount outside a rail's bounds refuses the order (`invalid_lightning_msat`,
   `invalid_onchain_sat`); and the line is `ALERT: fiat pricing is unavailable for a currency` with `asset`,
   `currency`, `reason` (`too_few` or `disagree`), `providers` (the names of the fresh sources) and, for
   `disagree`, `low` and `high` (the names of the providers with the lowest and the highest rate), logged on each
   change of state. It carries names only, never a rate.
2. **A stale rate.** Mitigation: `max_age_seconds` (default 300): a rate older than that, or stamped more than 60 s in
   the future, is not used; the fetcher runs every `fetch_seconds` (default 60, at most half of `max_age_seconds`).
   Kraken and Coinbase carry no timestamp (*source*:
   https://docs.kraken.com/api-reference/market-data/get-ticker-information.md and
   https://docs.cdp.coinbase.com/coinbase-app/track-apis/exchange-rates.md; *observed*), so their rates are stamped
   with the fetch time: for them the age rule measures the time since the fetch, not the age of the data, so a frozen
   or replayed rate passes it, and only the spread bound catches that. Coinbase's answer may itself be up to about
   60 s old (*inferred* from its cache headers `grpc-metadata-cache-control: public, max-age=60` and
   `cf-cache-status: HIT`, *observed*). Yadio stamps its rates in milliseconds (*source*:
   https://yadio.io/docs/openapi.json), and its `timestamp` ran about 3.5 minutes (210 s) behind the request
   (*observed*). With the defaults, a rate is read for up to 60 s after its fetch, so the observed lag gives about
   210 + 60 = 270 s, under the 300 s bound: Yadio counts, with little room, and drops out once its lag passes about
   240 s, and the order is then priced from the others (*inferred*). As built: a rate's age is measured from its
   provider's timestamp (Yadio's in milliseconds, the custom API's in seconds) or from the start of the fetch pass
   (Kraken, Coinbase), at the moment an order is priced; `fetch_seconds` is 10 to 600 and at most half of
   `max_age_seconds` (30 to 3600), checked at startup. `docs/rules-fiat.md` states the Yadio lag for the operator
   ("Rules for fiat pricing"). Residual: the rate is locked for the order's
   life (`expires_in`, 5 minutes to 24 hours), so the price can move against the merchant within it; the merchant
   picks the expiry; the margin absorbs some of it. An on-chain payment first seen before the expiry counts at the
   locked rate even when it confirms later, inside the order's 7-day watch window (§3).
3. **New outbound destinations.** Mitigation: four fixed hosts (`api.kraken.com`, `api.coinbase.com`,
   `api.yadio.io` and `api-pub.bitfinex.com`, which plan 8 added) and the custom URL, over
   HTTPS only (TLS 1.2 or later), with no redirect followed, no proxy from the
   environment, a 10 s timeout, at most 256 KiB read, and `User-Agent: SatBooth`. The host firewall's egress
   allowlist gains them (the operator's job; `docs/configuration.md` lists them under "The `rates` block"). Nothing from
   a provider's body reaches a log, an error or a response: the log line names the provider, an error class and the HTTP
   status. As built: one `http.Client` from the standard library for every provider, with `Proxy` nil, TLS 1.2 as the
   minimum, `CheckRedirect` returning the 3xx answer (counted as a transport failure), a 10 s client timeout (and an 11
   s bound around each call should a provider ignore it), at most 8 KiB of response headers, and idle connections closed
   after 30 s; a URL that is not `https` is refused again inside the request function, before any request. The line is
   `rate fetch failed` (a warning on the first failure of a provider's run of failures, then debug) with `provider`,
   `class` (`transport`, `status`, `too_large`, `shape` or `vendor`) and `status`; errors are fixed classes, never Go's
   transport error, whose text holds the URL. Residual: the providers learn SatBooth's address, a fixed user agent and
   the fetch interval, never an order or an amount (the requests carry only the asset and the currency codes). A
   provider that is down, slow or redirecting gives no rate, which is threat 4's outcome.
4. **Order floods as provider floods.** Mitigation: the fetcher makes one request per provider per interval; order
   creation reads the cache and makes no request. Yadio allows 100 requests a minute (*source*:
   https://yadio.io/docs/openapi.json); the others' limits for unauthenticated calls are not documented (Kraken's and
   Coinbase's pages give per-key limits only: *source*: https://docs.kraken.com/exchange/guides/rest/ratelimits.md,
   https://docs.cdp.coinbase.com/coinbase-app/api-architecture/rate-limiting.md). Residual: a provider that throttles
   or blocks SatBooth's address stops giving fresh rates; with fewer than `min_sources` fresh, fiat orders are refused
   (`rate_unavailable`, fail closed) and explicit orders still work. As built: one pass every `fetch_seconds`, all
   providers in parallel, passes never overlap, and a creation reads only the cache.
5. **The custom rate API is the operator's trust root.** Mitigation: it is one provider among the enabled ones, under
   the same rules; its URL is validated at startup (HTTPS, a host, no user info, no fragment, at most 512 bytes); its
   format is SatBooth's (`asset`, `rates`, `timestamp`). It never prices alone: `min_sources` is at least 2 (plan 7,
   decision 4), so the custom API is always checked against another source. A URL that carries a token in its query
   lives in `rates.custom_url_file`, a file with mode 0600 or stricter loaded in-process like the other secret files
   (`rates.custom_url` in the configuration file refuses a query, and setting both is refused), so the configuration
   file holds no secret. The URL is never logged, nor put in an error. The URL may name any HTTPS host, the private
   network included, and only the operator's configuration sets it (a request forgery, SSRF, needs the configuration).
   Pointed at an internal service, the worst it does is one blind GET per interval: a fixed path, no redirect followed,
   the body used only if it parses as SatBooth's shape, and never shown in a log, an error or a response; and most
   internal services do not speak HTTPS. As built: `min_sources` is 2 to 4, and the providers plus the custom API must
   number at least `min_sources`, checked at startup and again when `service.Run` builds the fetcher (so a configuration
   built in code cannot let the custom API price alone); the format is parsed strictly (an unknown or missing key, a
   duplicate key or another asset refuses the whole answer). Residual: a compromised custom API can block fiat pricing
   (threat 1's fail closed), and moves the median only within the spread bound; its host, its TLS and its availability
   are the operator's.
6. **The rate as a record.** Mitigation: every priced order stores its price, currency, the median, each source's rate
   and time, and the margin, immutably (`order_quotes`, insert-only like the orders): the amount can be audited after
   the fact. As built: the table is `order_quotes` (migration 4), one row per priced order, inserted in the order's
   own transaction (an order is never stored without its quote), with triggers that refuse an update or a delete and
   a row for an unknown order; the API's `pricing` member and the webhook body carry it. Residual: the record shows
   which rate was used, not that it was right, and it cannot stop a wrong price;
   a full compromise of the server (§0) can rewrite the store file.
7. **Terms.** Yadio asks to be cited with a link wherever its data is used or displayed (*source*:
   https://yadio.io/terms.html). `ACKNOWLEDGEMENTS.md` credits it. The checkout page shows a small "Rates: yadio.io"
   link only when Yadio was among that order's quote sources (only `yadio.io` is the link), and `docs/configuration.md`
   tells the operator that the same applies wherever else they show the data (`pricing.sources` names it). The operator
   checks the market-data terms of Kraken, Coinbase and Bitfinex for their own use. The repository's recorded Kraken and
   Coinbase answers (`rates/testdata/`, parser fixtures) are trimmed to what the tests read (Kraken's seven pairs,
   Coinbase's seven codes and its longest fraction) and credited in `ACKNOWLEDGEMENTS.md`. Residual: terms can change;
   Yadio may modify, suspend or terminate access at any time, without notice (*source*: https://yadio.io/terms.html),
   which is threat 4's outcome.
8. **Configuration.** Mitigation: a negative margin is a discount and is explicit (`margin_percent`, a decimal string,
   -50 to 50 with at most 2 decimals); a priced amount outside a rail's bounds is refused, never clamped. As built:
   the bounds are as written here (also `max_spread_percent` 0.01 to 50 and `min_sources` 2 to 4), each refusal
   names its key and never the value, and a JSON number in a percentage is refused. Residual: a
   margin inside the bounds can still be a typo (`-5` for `5`), and `max_spread_percent` (up to 50) and
   `max_age_seconds` (up to 3600) let an operator weaken threats 1 and 2's protections within their bounds; the
   operator reviews the configuration, and the quote record (threat 6) shows the margin used.

## 11. Several webhook endpoints (plan 13)

As built. The single merchant webhook (`webhook`) becomes a list of one to eight endpoints
(`webhooks`, each with a `name`, a `url` and a `secret_file`); `design.md` §3, "Several webhook endpoints (plan 13)",
has the design. Webhooks carry no money and no secret, but they are, after the API, the second way in which order
data leaves SatBooth, and an endpoint is a receiver the operator has to trust. These are the rules.

1. **Where requests go.** The outbound destinations of the public host are the wallet relay, the rate providers (with
   the `rates` block), the alert URL (with the `alerts` block) and up to eight webhook URLs. All of them come from the
   operator's configuration file; no request input, order field or event chooses a destination. Each endpoint's
   client follows no redirect and uses no proxy from the environment, with the 10 second timeout of a webhook (§5),
   so a receiver that answers with a redirect leads nowhere. The deliverers open a new connection for each request
   (no kept-alive connections), and the process's standard logger, the only place the HTTP library writes on its own
   (for example the first bytes a receiver sends unasked), passes through a writer that logs one fixed line and none
   of the bytes a peer sent. The configuration refuses a list shorter than one or
   longer than eight and the old key `webhook` (with a message that names `webhooks`).
2. **One secret per endpoint.** Each endpoint is signed with its own secret, with the signature of today
   (`SatBooth-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256>`, §3). No two endpoints share a secret, and none
   shares the alert receiver's, compared as HMAC keys (a secret that is another followed by zero bytes is the same
   key and is refused too). So the holder of one endpoint's secret, whether a receiver that leaks it or a third party
   that runs the receiver, can forge events to that endpoint's receiver only: not to another endpoint's and not to the
   alert receiver.
3. **Every endpoint sees every event.** There is no per-endpoint filter, so an endpoint is as trusted with order ids,
   event types and prices as the merchant's application is. A logger or an accounting tool run by a third party
   learns all of them. The body is the one of today (§3). The operator chooses the endpoints; a person who can only
   reach an endpoint's receiver cannot add one.
4. **What history an endpoint can receive.** The store records each endpoint's first delivery pass in this data
   directory (a row per name, written once). An endpoint is admitted the events created at or after 24 hours before that
   time, and never an event older than the retry window plus seven days. The first pass is stamped by the clock of the
   moment, so the bound is capped by the current clock: an event is admitted from the later of `now − 24 h − 7 d` and
   `min(first pass, now) − 24 h`. An event that already has an attempt for the endpoint stays listed until it is
   delivered or abandoned, whatever that floor. An event below the floor that has no attempt for the endpoint is not
   listed for it, so it is sent nothing, logs no line and raises no alert. For the reader:
   - A name that is new to the data directory is sent the events of the last 24 hours and nothing older, so a receiver
     added today does not receive the history. It does receive the last 24 hours, including events from before it was
     added: adding an endpoint hands it that day. The limit also keeps a mistyped or unfinished endpoint from being
     sent, and from logging as abandoned, a week of old events.
   - `main`, in a store that had events before the change, has its first pass at the oldest event's time, so it
     continues as the single webhook did. In a store without events `main` is recorded at its first pass, like any name.
   - A name removed and added again keeps its first pass. It can be sent the events up to that bound that it never
     tried, with one last attempt for an event older than 24 hours. That is the most history any endpoint can receive.
   - The cap and its residuals. A first pass stamped by a clock that ran ahead cannot hide an event that is inside its
     own 24 hours by the current clock, and cannot take away the last attempt or the abandoned line of an event the
     endpoint has already tried. The residuals: (a) an event the endpoint never tried, if the clock ran ahead at the
     first pass and the service was down (or the event otherwise had no attempt) until the floor passed it, gets no
     attempt and no line for that endpoint; (b) a clock that ran behind at the first pass lets a new endpoint be sent
     more than 24 hours of events, up to the seven-day bound, with an abandoned line for each that fails.
5. **A dead endpoint costs one request per retry step and nothing else.** Each endpoint has its own attempts,
   retries, 24 hour limit and "abandoned" line. An endpoint that never answers is tried at the steps of the retry
   policy (10 seconds, doubling to 1 hour) for 24 hours per event, and then each event is abandoned with one `ALERT`
   line that names the endpoint. It never delays another endpoint's delivery: a slow endpoint holds up its own
   requests only. A deliverer reads the store when it is woken, at a retry or a deadline, and, when idle, at most 30
   seconds after its last pass ended, not every second (plan 14).
   The "abandoned" line is durable (plan 14): the last failed attempt and a mark "abandoned, not yet announced" (the
   table `webhook_abandoned`) are one write, and a pass logs the line of every marked event and then removes the
   mark, so the line is logged at least once across a crash or a restart, and once per run; a crash between the line
   and the removal can repeat it after the restart. A removed endpoint's marks stay until it returns. Events abandoned
   before the upgrade have no mark and no line. A mark that cannot be removed is logged (a fixed line) and does not stop
   the endpoint's deliveries. A 2xx that could not be recorded is kept in memory, per endpoint and event, and only its
   record is retried until it succeeds or the event leaves the delivery window (never by its age alone, since the last
   attempt and the first one after a long downtime come after the 24 hours), so the same process neither posts it again
   nor marks it abandoned; after a restart it is posted again, as the at-least-once promise says.
5a. **A wake-up says when to look, never what to send** (plan 14). An event or an alert is looked for as soon as it is
   stored: the store wakes the event deliverers after each new event, and the alert deliverer after each new alert row,
   once the write has ended, and the deliverer then lists what is undelivered from the store, as it did on every pass
   before. The wake-up holds no event, so one that is lost, merged with another or never sent costs time and nothing
   else: each deliverer also passes 30 seconds after its last pass ended, so a missed signal costs at most that long of
   idle waiting, and a restart lists everything from the store. A pass lasts as long as its requests, which go one at a
   time, each with its 10 second timeout, and an event stored while a pass runs is picked up by the next pass: a slow
   receiver delays its own later events only. Passes are at least one second apart, as before, so a burst of events
   cannot make a deliverer scan faster than it did when it polled. The retry steps, the 24-hour limit, the abandoned
   line, the request, the body and the signature are unchanged. The alert deliverer is woken the same way. A clock
   stepped forward makes a due retry up to 30 seconds late; a clock stepped back changes nothing that the stored times
   did not already. The signal is in memory and per process; one process owns a data directory, so every event and alert
   that a running deliverer can see is written by that process.
6. **Names.** An endpoint's name is 1 to 64 characters of `[A-Za-z0-9._-]` and is unique in the list, so a name is
   safe in a log line, an alert field and a store column. A removed endpoint is no longer delivered to and its
   recorded attempts stay (an immutable record, as every table, §3). A name added again is the same endpoint and
   continues under the rules after downtime: what it was sent is not sent again, what it abandoned stays abandoned,
   and an event it never tried gets its attempts. A renamed endpoint is a new endpoint with its own first pass.
   Attempts recorded before the change belong to the endpoint `main`, the one the single webhook becomes.
7. **Logs and alerts.** URLs are never logged and never part of an error, since a URL may carry a token in its path
   or query; a log line and an alert name an endpoint by its `name`, and the deliverers' constructor does too (or by its
   position, `endpoints[2]`, when the name itself is bad). A configuration error names an entry by its position in the
   list, counted from 0 (`webhooks[2].url`), even when the entry has a name. The alert allowlist gains the key
   `endpoint` (a name, shaped as above), so two endpoints that abandon one event are two alerts and not one alert with a
   repeat. The line `ALERT: a webhook event is held back: its order's stored quote is malformed` is about the event, not
   an endpoint: one line and one alert per event whatever the number of endpoints.
8. **The alert receiver stays single** (§9). It is not an endpoint of the list, has its own secret, and a URL that
   equals any endpoint's is refused.

**Residual risks.**
- An operator who reuses one secret under two names is refused only by the equality check at start (the secrets are
  compared as HMAC keys). A secret kept by hand in two places outside SatBooth's view cannot be detected. Two
  receivers that share a secret can forge to each other.
- A webhook URL with a token in its path or query is a secret of the operator. SatBooth never logs it; the
  configuration file, which holds it, is the operator's to protect, and so is every backup of it.
- An endpoint's name is in the logs and in the alert sent to the operator's receiver, which is the operator's
  monitoring and may be run by a third party. Names should not be secrets or personal data.
- An endpoint's name is not in the body or a header, so a receiver does not learn the other endpoints' names. A
  receiver that is slow or hostile cannot make SatBooth send it more than the retry policy allows for each event.
- An attacker who holds the server (§0) holds every endpoint's secret and can forge events to every receiver, as with
  one secret; several endpoints do not make that worse, but a leak of the configuration directory leaks all of them.
- A new endpoint is sent up to 24 hours of events in one burst. A receiver that cannot take it slows only itself.
