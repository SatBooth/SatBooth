# Backup and restore

`satbooth backup -config <path> -to <dir>` copies the two databases as one matched pair. A backup is offline: the
service must be stopped, and the command refuses unless it can take the data directory's lock. Stopping SatBooth for a
few seconds is the cost of a pair that is consistent by construction.

- It refuses if `<dir>` exists or is inside the data directory, and creates it with mode 0700.
- It refuses, as a start does, when `store.db` or `ledger.db`, or one of their `-wal` and `-shm` files, is a symbolic
  link, a hard link or not a regular file: two data directories, each with its own lock, must never share a database.
  A snapshot tool that hard-links the data directory's files (`cp -al`, rsnapshot, `rsync --link-dest`) therefore
  stops every start until its links are removed.
- It refuses unless the pair stamp (see "The pair stamp") shows equal ids, equal generations of at least 1, equal
  nonces and no advance pending. A pair that does not match gets the same refusal as a start. An incomplete state (an
  advance pending, a first stamp, generation 0) gets "start SatBooth once to finish its last start": start SatBooth
  once, stop it, and run the backup again.
- It never migrates, and it changes no row and no schema in the production files; SQLite may checkpoint the WAL. A
  database whose schema is older than the binary's gets the error "start SatBooth once to upgrade it".
- It writes `store.db` and `ledger.db` (each made with SQLite's `VACUUM INTO`, mode 0600) and then `manifest.json`
  (mode 0600), which holds the format `satbooth-backup/v1`, the creation time (Unix seconds), the pair id, the
  generation, and the size and SHA-256 of each file. Each copy passes SQLite's `quick_check` before the manifest is
  written, and a backup that fails leaves no `manifest.json`: a directory without one is not a backup.

      {"format":"satbooth-backup/v1","created_at":1700000000,"pair_id":"<32 hex digits>","generation":7,
       "files":[{"name":"ledger.db","bytes":0,"sha256":"<64 hex digits>"},{"name":"store.db","bytes":0,"sha256":"<64 hex digits>"}]}

- On success it writes only one fixed line with the generation: a JSON log line on stderr, like the service's logs.
  Exit code 0 is success, 1 a failure, 2 a usage or configuration error.

In the public host's Compose bundle the backup runs in a one-off container of the image, without a network, into the
`SATBOOTH_BACKUPS` directory (`deploy/README.md`, "Backups").

To restore:

1. Stop SatBooth.
2. Remove the data directory's `store.db*` and `ledger.db*`.
3. Copy both files from one backup directory into the data directory.
4. Start SatBooth. **With a `payout` block (Lightning refunds), not yet: do "With refunds on" below first.**

In the public host's Compose bundle the data directory is a named volume and the image has no shell: the commands are
in `deploy/README.md`, "Restore". Restore both files from the same backup. There is no command and no override for a
pair that does not match, on purpose: a ledger and a store from different moments disagree about which orders were
paid, and any way to start on them would let SatBooth serve and settle on a wrong record of the money. The two files
of one backup are equal by construction, so the start-up check passes.

**With refunds on (a `payout` block).** A restore puts the store back to the moment of the backup. "This refund was
already claimed" is recorded only in the store (`refund_exists`), and an approval stays valid until its own
`expires_at`, at most 30 days after it is checked. An approval signed before the restore, for a refund claimed after
the backup, could be claimed again with a new invoice and paid a second time, within its amount, `payout.max_msat`,
the daily cap and the payout connection's budget. One rule closes it:

1. **Before the first start after the restore, replace `payout.approval_keys` with new public keys only, and keep the
   `payout` block.** The admin system makes the new key with `satbooth-approve keygen -out <a new path>` (it refuses an
   existing path and prints the public key; see [`satbooth-approve`](refunds.md#satbooth-approve)). The private key stays with the admin and signs
   no refund approval yet. Destroy the old signing keys. (A start without the block adds nothing to this: it is refused
   while the restored store holds a refund with no result, and it turns the refund read endpoint into 404.)
2. Start SatBooth. An approval signed with a key that is no longer listed is refused at claim (`invalid_approval`).
   For each refund of the restored store:
   - `claimed`: it ends `failed` before anything is sent, with the reason `key_revoked`, or `expired_unsent` when its
     approval has expired;
   - `sent`: it is looked up at the hub. The hub's `settled` or `failed` ends it. `pending` leaves it open (an `ALERT`
     once an hour has passed since the send) and a void is refused until the hub settles or fails the payment. No
     record, or `expired`, logs an `ALERT` and leaves it open until a void approval, signed with a new key, fails it
     with the reason `voided` (accepted from 11 minutes after the send). An answer that does not fit the refund (not an
     outgoing payment, another invoice or amount, or a malformed one) logs an `ALERT`, leaves it open and makes a void
     answer `hub_unavailable`: a person investigates (see [Voiding a refund](refunds.md)). The hub unreachable: it stays open and
     is looked up again; a void answers `hub_unavailable`.
3. **Approve no refund with the new key until the payouts made after the backup are reconciled** (a void approval
   moves no money and may be signed at any time). The restored store and ledger do not hold them, and `Claim` checks the
   refundable amount against what they hold. Before reconciling, wait at least 11 minutes after the old SatBooth
   stopped or its host went down, whichever is later (when unsure, count from the time you are sure). The hub does not
   read a request's expiration: only SatBooth's relay, on the hub host and by that host's clock, stops serving a pay
   request past its send time plus `payout.timeout_seconds` (10 to 600). The 11 minutes are the longest timeout, 10
   minutes, plus a minute for the difference between the two hosts' clocks; both hosts must keep time with NTP, and
   neither clock may have been stepped by hand. Otherwise a request sent before the restore can still reach the hub,
   and it and the replacement refund would both pay. A payout the hub still shows `pending` counts as paid until the
   hub shows it `failed`: reconcile again when it has resolved, and approve nothing for that order before. The wait
   does not cover a hub that was stopped or cut off while it held such a request: it can still pay it when it resumes,
   and the list shows nothing before. Before approving anything for an order with a refund in the hub's list that the
   restored store does not know, reconcile again. The time to start from is the backup's own:
   `created_at` (Unix seconds) in its
   `manifest.json`. In the hub, list the payments the payout connection made at or after that time. The refund view
   never carries the invoice, so match each payment to the claim that carried its invoice, from the admin system's or
   the merchant app's own record of its claims. For a refund the restored store does not know, or shows `failed`
   (`key_revoked`, `expired_unsent`) or open although the hub paid it, count it as paid in every later approval for that
   order.
4. Only then may the admin system sign refund approvals with the new key.

The correction lasts as long as the order. For an order whose refund was paid after the backup, the restored store and
ledger never learn the payout: the order's `refunded_msat` stays too low and `exceeds_refundable` does not stop a
refund that repeats it, and nobody corrects either by hand. The admin must subtract that payout from what it approves
on that order, for good.

**What else a restore takes back.**
- Orders, invoices and payments recorded after the backup are gone. An order made after the backup is unknown to the
  restored store, and a payment to its invoice, address or subaddress is not recorded and not alerted (outputs to an
  address the store has no row for are skipped). Look for them in the hub's incoming payments after `created_at` that
  match no invoice in the store, in bitcoind's receives to addresses whose index is above the store's highest, in
  wallet-rpc's transfers to subaddresses above the store's highest, and in the webhooks received after `created_at`,
  and handle them by hand.
- Payments made after the backup, by what the backup holds. A payment to an invoice, address or subaddress that the
  backup holds is found again; one to an invoice, address or subaddress issued after the backup is not, even when its
  order is in the backup: look for it as for a lost order. A Lightning invoice that is still open is looked up and
  recorded paid. On-chain and Monero, a payment that is found again is first seen at the restore. If its order had
  expired by then and the restore falls inside the order's 7-day watch window, it is recorded late (`order.late` and
  an `ALERT`); past the window it is not recorded, and only an `ALERT` that it arrived after the watch window names
  its order. If the order had not expired yet, it counts as an on-time payment and `order.paid` goes out again, with
  the same `id` as before the restore (an event's id is derived from its type and its order), so a receiver that
  deduplicates on `id` ignores it. For a payment recorded late or named by the after-window `ALERT`, the merchant may
  already have had `order.paid` before the restore: compare with the webhooks received since `created_at` before
  crediting or refunding it by hand.
- The addresses and subaddresses issued after the backup are not issued again while the wallets keep their state:
  the next index comes from bitcoind's wallet (`getnewaddress`) and from wallet-rpc (`create_address`), and SatBooth
  refuses an index that is not above the highest its store records. **If a wallet is rebuilt as well, its next index
  must be set above everything issued since the backup**, which the restored store no longer knows: an address shown
  to the customer of an order the restore lost must never go to a new order, because a late payment to it would mark
  the new order paid. For bitcoind, see [Setting up bitcoind](bitcoind.md), step 6; for Monero, [Setting up monerod and
  monero-wallet-rpc](monero.md), step 5 of the start-up checks.

## The pair stamp

Each database holds a `pair` table with one row: a random id (32 lowercase hex digits, set once), a generation number
and a nonce (32 lowercase hex digits, none at generation 0). The ledger's row also holds a `pending` generation and its
nonce. The first start after the upgrade writes one fresh id into both databases, ledger first, and a fresh install
takes the same path. That start, and the start that finishes an interrupted first stamp, then advances the generation
in the same start, so no completed start leaves a generation at 0. Every start advances the generation of both
databases by one and writes one fresh random nonce with it into both, in an order that a crash cannot leave
unrecoverable: the ledger is marked `pending` (the next generation and its nonce), then the store is written, then the
ledger is committed.

At every start, after the migrations and before any worker, recovery or listener, SatBooth reads both rows and allows
exactly these states (the exact table is in `docs/design.md` §3, "Operations, as built (plan 10)"): neither database
stamped yet; the first stamp half done (ledger stamped, store not), both generations 0; equal ids, equal generations
and equal nonces with no advance pending; and the two states a crash between the three writes of an advance leaves,
which the start finishes only with the nonce the ledger recorded for that advance, and then advances once more with
a fresh nonce. Anything else is refused: SatBooth logs a fixed message, exits 1, and says which file is older (by
generation), or that the two are not a pair when the ids differ or the generations are equal with other nonces.
This catches a store restored without its ledger, or the
reverse, or from another installation. It also catches a mix of two timelines: after a restore the next start reaches
generations that backups made after the restored one already have, with other content; those starts drew other nonces,
so a file from such a backup next to a file of the restored pair is refused. A `store.db` or `ledger.db` that was lost
or deleted comes back as a fresh empty file with no id and generation 0, and is refused the same way: it is never
taken for a first stamp. The remedy is the restore above: both files from one backup. A refusal is never overridden.

The stamp does not catch a pair restored together from before the stamp existed (both generation 0, no id), or a
file from before the stamp next to a recreated empty one, before the first start that stamps them: either is stamped
as a first start. It does not compare the content of the two databases either. So a hot copy of a single file (taken
while SatBooth runs) is not a backup: restored later against the other file at the same generation, it passes the
check while its content disagrees. A backup is offline for this reason.
