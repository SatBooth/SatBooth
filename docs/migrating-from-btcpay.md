# Migrating from BTCPay Server

This guide is for a merchant who runs BTCPay Server today and wants to take payments with SatBooth. It is a guide
only: SatBooth has no migration tool and no BTCPay API compatibility layer. You set up SatBooth beside BTCPay, change
your application to call SatBooth's API, switch new orders over, and then retire BTCPay.

Check the status note at the top of [`README.md`](../README.md) before you move real money.

## 1. What moves and what does not

Moves:

- Taking payments over Lightning, on-chain bitcoin and Monero.
- Your application's integration: it creates orders, sends customers to the checkout page and reacts to webhooks.
- Refunds over Lightning (section 6).

Does not move:

- **Invoice history.** SatBooth starts empty. Export what you need from BTCPay before you stop it.
- **BTCPay's apps** (point of sale, crowdfunding, pay button), payment requests and pull payments. SatBooth has no
  equivalent. Refunds of on-chain and Monero payments are paid by a person from your own wallet, never by SatBooth.
- **Stores.** SatBooth is one merchant: it has no stores.
- **Lightning channels and their funds.** They stay in the old node until you close them or drain them (section 4).

## 2. Before you start

- **Two hosts.** The hub host is private; the public host runs SatBooth's checkout page. The hub never shares a host
  with the public-facing service, and the two hosts talk over a private link you provide. Both hosts are set up with
  `satbooth-setup`; follow [`deploy/README.md`](../deploy/README.md#set-up-with-satbooth-setup) for the steps and for
  the private link.
- **SatBooth runs its own bitcoind**, dedicated to the hub and SatBooth, in the hub host's bundle. Do not point
  SatBooth at the bitcoind of your BTCPay node. SatBooth's RPC user is confined by `rpcwhitelist`, but the whitelist
  is per method, not per wallet: a node that also serves a wallet that holds keys lets a stolen SatBooth password
  read that wallet's keys ([Setting up bitcoind (on-chain)](bitcoind.md)).
- **SatBooth's hub is a new Alby Hub of its own** (LDK backend). Using an existing Alby Hub is not supported, and a hub
  on top of your old LND or Core Lightning node is not supported either.
- **BTCPay keeps running** until the cut-over (section 7). Nothing in this guide touches it before then.

## 3. On-chain

SatBooth watches a descriptor you give it: one extended public key and no private key. It refuses anything with private
key material.

**Make a new account, with its own extended public key, in the same wallet. Never give SatBooth the extended public
key of your BTCPay store.** BTCPay issues addresses from that key. SatBooth's rule is that no other wallet may issue
addresses from the chain it watches: a wallet that hands out addresses from the same `<key>/0/*` reuses SatBooth's
addresses, and its receipts count toward SatBooth's orders ([Setting up bitcoind (on-chain)](bitcoind.md), step 1).

Which BTCPay stores have a SatBooth form depends on the store's derivation scheme (NBXplorer, `docs/API.md`, commit
a7f2723, "Standard derivation schemes"). SatBooth takes `wpkh(KEY/0/*)` or `tr(KEY/0/*)`
([Configuration](configuration.md), `descriptor`).

| BTCPay derivation scheme | Address type | SatBooth |
|---|---|---|
| `xpub...` | P2WPKH | `wpkh(KEY/0/*)` |
| `xpub...-[taproot]` | P2TR | `tr(KEY/0/*)` |
| `xpub...-[p2sh]` | P2SH-P2WPKH | no form |
| `xpub...-[legacy]` | P2PKH | no form |
| `2-of-xpub1...-xpub2...` and the other multisig forms | P2WSH, P2SH-P2WSH, P2SH | no form |

For a store with no form, make a new native-segwit or taproot single-signature account and use that. NBXplorer
tracks receive addresses at `0/i`, which is the chain `KEY/0/*` names. A store set up from a descriptor (NBXplorer calls
them policy schemes) maps the same way by its script type: a single-key `wpkh` or `tr` has a SatBooth form, and anything
else (a Miniscript policy, `wsh`, several keys) has none.

Make the account in the wallet that holds the keys, export its extended public key with its key origin, and follow
[Setting up bitcoind (on-chain)](bitcoind.md) for the rest: the RPC user, the watch-only wallet, the descriptor import
(import exactly one receive descriptor) and raising your wallet's gap limit. The hub host's bundle runs the bitcoind.

## 4. Lightning

SatBooth's hub is a new node with its own channel. Design (`docs/design.md` §2.1): one channel, opened from your main
node or an LSP, kept small, and swept back to your main node regularly by paying an invoice from that node. A hub on
top of the old LND or Core Lightning node is not supported.

Two ways to connect your old node to the new hub:

1. **Open a channel from the old node to the hub.** The hub then has inbound capacity from a node you already run.
   Keep the channel small: the money at risk on the public side is bounded by the hub's channel size and its payout
   budget ([The idea in one table](../README.md#the-idea-in-one-table)). Sweep what the hub receives back to the old
   node.
2. **Open the hub's one channel from your main node or an LSP**, and close the old node's channels when you retire it.

Either way the old node keeps its other channels and their funds until you close or drain them.

## 5. Monero

BTCPay's Monero support is a plugin that builds a wallet from a primary address and a private view key, and that
shares one wallet across all stores (Monero plugin, `btcpay-monero/btcpayserver-monero-plugin`, commit dc273d2:
`README.md`, `Plugins/Monero/Controllers/MoneroLikeStoreController.cs`, which calls wallet-rpc's `generate_from_keys`
with a primary address, a private view key and a restore height). SatBooth works the same way: the same primary address
and private view key make SatBooth's view-only wallet in monero-wallet-rpc on the hub host. Give the restore height as
the height at which the wallet was made ([Setting up monerod and monero-wallet-rpc (Monero)](monero.md), step 3).

Two rules from that section matter during a migration:

- The private view key shows every incoming payment of the whole wallet, all its accounts. Use a wallet that is for shop
  income only.
- No other wallet may hand out subaddresses of the account SatBooth uses (`account_index`): SatBooth issues them itself,
  one per order, and an index you hand out that has not been paid yet is issued again to an order. While BTCPay still
  takes Monero payments, make sure its subaddresses and SatBooth's do not come from the same account.

SatBooth never holds a Monero spend key, and the hub host's bundle runs monerod and wallet-rpc with its `monero`
profile.

## 6. The merchant's integration

You change your application to call SatBooth's API ([The merchant API (private listener)](merchant-api.md)). The API is
on a private listener that the bundle does not publish. Your application reaches it from the public host itself or from
a container attached to the bundle's network; an application on another host needs a way in that the bundle does not
open, which is the operator's decision (`deploy/README.md`, "The merchant API"). The checkout page is public.

(BTCPay Server v2.4.7, `swagger.template.invoices.json` and `swagger.template.webhooks.json`; the refund flow, BTCPay's
`Refund.md`.)

| | BTCPay Server (Greenfield API) | SatBooth |
|---|---|---|
| **Create** | `POST /api/v1/stores/{storeId}/invoices` | `POST /v1/orders` |
| Amount | `amount`, a decimal string in `currency`; null makes a top-up invoice | Explicit per rail, as JSON integers in the rail's smallest unit, not strings: `lightning_msat` (msat, a whole number of sats, so a multiple of 1000), `onchain_sat` (sat), `monero_piconero` (10^12 per XMR, at most 2^53 - 1); give any of them, **or** a fiat `price` (a JSON string) with `currency` and optional `rails`. Never both (400 `mixed_pricing`). No top-up invoices. A fiat `price` needs the operator's `rates` block, which converts it at order creation and locks the rate ([Rules for fiat pricing](rules-fiat.md)) |
| Currency | `currency`; the store's default if left out | `currency`, 3 uppercase letters, one of the operator's `rates.currencies`; only with `price` |
| Your order reference | `metadata.orderId` (the `redirectURL` description names it); not documented as unique | `merchant_ref`, required, 1 to 64 characters of `[A-Za-z0-9._-]`, **unique across all orders and idempotent**: the same reference with the same account and amounts (or the same `price` text, currency and rails) returns the existing order; anything else is 409 `conflict` |
| Customer | none required | `account_id`, required, 1 to 64 characters of `[A-Za-z0-9._-]` (anything else is 400 `invalid_account_id`); use a stable id per customer (SatBooth's limits are keyed on it) |
| Expiry | `checkout.expirationMinutes`; the store default is 15 | `expires_in`, 300 to 86400 seconds, default 3600 |
| Payment methods | `checkout.paymentMethods` | Explicit amounts: the rails you give an amount for. Fiat price: `rails` (Lightning, plus on-chain when the operator set the `onchain` block, **never Monero unless `rails` names it**) |
| Confirmations | `checkout.speedPolicy`, per invoice | Not set per order: SatBooth picks the tier (1, 3 or 6 confirmations by default for bitcoin from `onchain_sat`; 1, 3 or 10 for Monero from `monero_piconero`), and a tier may be 0 ([Rules for on-chain payments](rules-onchain.md)). The operator configures the tiers |
| Other options | `checkout.paymentTolerance`, `checkout.redirectURL`, `checkout.monitoringMinutes`, `receipt` | SatBooth has no equivalent |
| What you read back | `id`, `checkoutLink`, `status`, `additionalStatus`, `paidAmount`, `expirationTime` | `id` (32 hex), `checkout_url`, `state`, per-rail objects, `created_at`, `expires_at` and `paid_at` (RFC 3339). 201 for a new order, 200 for a repeat |
| **Read** | `GET /api/v1/invoices/{invoiceId}` | `GET /v1/orders/{id}`: the same object the `POST` returned |
| List, search, archive, update | `GET /api/v1/stores/{storeId}/invoices`, `DELETE` and `PUT` on an invoice | SatBooth has no equivalent: there is no list call. Keep the order `id` with your own record |
| **Checkout link** | `checkoutLink`, a page that shows the payment options | `checkout_url`, `<checkout_base_url>/c/<token>`: a page with no JavaScript that refreshes itself every 5 seconds. The customer presses "Pay" (or "Pay with Monero") to get an address, invoice or Monero subaddress, one of each per order |
| **Status** | `status`: `New`, `Processing`, `Expired`, `Invalid`, `Settled`; and `additionalStatus`: `None`, `PaidLate`, `PaidPartial`, `Marked`, `Invalid`, `PaidOver`. The API description says `additionalStatus` "describes why an invoice is in its current status"; BTCPay's guide says to check both | `state`: `new`, `paid`, `late`, `underpaid`, `expired`. Per rail: `lightning.state` (`new`, `paid`, `expired`); `onchain.state` and, with the same values, `monero.state` (`waiting`, `seen`, `paid`, `overpaid`, `late`, `underpaid`, `expired`), with `overpaid` and `stale` flags on both |
| How the statuses correspond | `New` | `new`. An order past its expiry stays `new` while a payment is still being checked; read it again until `onchain.stale` (or `monero.stale`) is false before you act on `expired` |
| | `Processing` (fully paid, not enough confirmations yet) | No state. The order stays `new`, with `onchain.state` `seen`, until the payment has its tier's confirmations; then it reads `paid` |
| | `Settled` | `paid` |
| | `Expired` | `expired`: nothing was received on any rail |
| | `Invalid` (the webhook description: the invoice "has received payments which could not confirm in time", unless it was marked by hand) | `underpaid`, with `order.underpaid`, when the watch window closes with on-time payments that reach the amount but are not confirmed enough. A payment that counted and was then taken away is a different case: `order.reverted` (no BTCPay status is forced onto it) |
| | `Marked` (set by hand) | SatBooth has no equivalent |
| | `PaidPartial`, `PaidLate`, `PaidOver` | Not one to one. `underpaid` (short of the amount, or not confirmed enough when the 7-day watch window closes), `late` (first seen at or after the expiry; a person decides) and `paid` with `onchain.overpaid` true (refund the extra). Read the definitions in [The merchant API](merchant-api.md) before you map them |
| **Webhook events** | `InvoiceCreated` | SatBooth has no equivalent |
| | `InvoiceReceivedPayment`, `InvoicePaymentSettled` | SatBooth has no equivalent: read the order |
| | `InvoiceProcessing` | SatBooth has no equivalent |
| | `InvoiceSettled` | `order.paid`, once per order |
| | `InvoiceExpired` | `order.expired`, once per order (it can be followed by `order.paid` on Lightning for up to about 24 hours, and on-chain for up to the watch window) |
| | `InvoiceInvalid` | `order.underpaid` for payments that could not confirm in time. `order.reverted` is separate: a reorg or, on a 0 tier, a payment gone from the mempool or tx pool took away payments that had counted, and the order no longer reads paid; it carries a `seq`, and `order.paid_again` follows if the order reads paid again |
| | none | `order.underpaid`, `order.late`, `order.paid_again` |
| Subscribing | Per store, in the UI or API: a URL, a chosen set of events or `everything`, an optional secret | A list of 1 to 8 endpoints, `webhooks`, in SatBooth's configuration, each with its own secret; every event goes to every endpoint |
| Body | `type`, `deliveryId`, `webhookId`, `timestamp`, `storeId`, `invoiceId`, `metadata` and more | `id` (the event id), `type`, `seq`, `order_id`, `created_at` (Unix seconds, unlike the order's RFC 3339 times), and `pricing` for a priced order. There is no `merchant_ref` in it: read the order to get it |
| **Signature** | Header `BTCPay-Sig: sha256=<hex>`: HMAC-SHA256 of the **raw body**, keyed with the UTF-8 text of the webhook secret | Header `SatBooth-Signature: t=<unix seconds>,v1=<64 lowercase hex>`: HMAC-SHA256 over `t`, a dot and the **raw body**, keyed with the **decoded bytes** of the secret (the secret file holds hex; keying with the hex text verifies nothing). Reject if `t` is more than 5 minutes from your clock. [Webhooks](webhooks.md) has a test vector |
| Secret | Generated by BTCPay if you leave it out (or you give one); the create response returns it | `satbooth-setup` makes it at stage 1, in `etc/webhook.secret` on the public host; after stage 4 and before the first start, give its value to your receiver (`deploy/README.md`, "Set up with `satbooth-setup`", step 5). It is 64 to 128 hex digits; the HMAC key is the decoded bytes |
| Delivery and retries | The API description: a failed delivery is retried after 10 seconds, 1 minute and up to 6 times after 10 minutes (`automaticRedelivery`, default true); BTCPay's source retries on 5xx, 429, 408 and when there is no response | At-least-once. Any 2xx is success; any other status, a 10 s timeout, a connection failure or a redirect is a failed attempt. Retries after 10 s, then 20 s, 40 s and so on up to 1 hour, until 24 hours after the event was created; then an `ALERT` is logged and it stops. Events can arrive out of order: re-read the order on every event, and deduplicate on the event `id` |
| **API key header** | `Authorization: token <key>` | `Authorization: Bearer <key>` |
| Permissions | `btcpay.store.cancreateinvoice` (create), `btcpay.store.canviewinvoices` (read), `btcpay.store.canmanageinvoicestatus`, `btcpay.store.canmodifyinvoices`, `btcpay.store.cancreatepullpayments` (refund), `btcpay.store.webhooks.canmodifywebhooks`. Keys are made by a BTCPay user | Capabilities: `orders:write` (create), `orders:read` (read), `refunds:write` (claim, void), `refunds:read`. **One capability per key.** `satbooth-setup` makes the keys at stage 1: the SHA-256 of each goes in `api-keys.json`, and the plain keys, one per file, in `setup/api-keys/` on the public host, which you hand to your application with the webhook secret (`deploy/README.md`, step 5). A key is any string of up to 512 bytes |
| **Refunds** | `POST /api/v1/invoices/{invoiceId}/refund` makes a pull payment. The customer opens its link and enters a destination (a payout); the merchant approves, signs and sends it from a wallet, on-chain or other methods | Lightning only, and only for orders paid over Lightning. A person signs an approval offline (`satbooth-approve refund -key <path> -network <name> -order-id <32 hex> -amount-msat <n> -expires-in <duration>`; `-amount-msat` is a multiple of 1000), in the admin system, with a key that never touches the SatBooth host. The tool prints the `approval` and a `refund_id`. Your app calls `POST /v1/refunds` with the approval and the customer's Lightning invoice, which must carry exactly the approved amount; it answers 202 and the refund is sent in the background. Poll `GET /v1/refunds/{refund_id}` with the printed `refund_id`: there are no refund webhooks. `POST /v1/refunds/{refund_id}/void` ends an open refund and needs its own signed approval (`satbooth-approve void`). The operator sets a lifetime budget for the payout connection in the hub |

Notes:

- **Merchant references.** In BTCPay your order id lives in `metadata`; in SatBooth `merchant_ref` is a field of its
  own.
  Store SatBooth's `id` next to your own order id when you create the order: webhooks carry `order_id`, not
  `merchant_ref`.
- **Retries are safe.** If a response to `POST /v1/orders` is lost, send the same request again: it returns the order
  the first call created. With a fiat price, send the same `price` text byte for byte (`"21.50"`, not `"21.5"`).
- **A webhook is a hint.** After you verify it, read the order and act on its `state`, not on the event's type.

### BTCPay features SatBooth has no equivalent for

- Stores and multi-tenancy.
- Apps (point of sale, crowdfunding, pay button) and payment requests.
- Pull payments and payouts beyond the Lightning refund above, and on-chain or Monero refunds paid by the server.
- Invoice listing, search, archiving, updating, and marking an invoice's status by hand.
- Top-up invoices (an invoice without an amount).
- Per-invoice confirmation policy, payment tolerance, a redirect URL, a monitoring time and receipts.
- Webhooks for invoice creation, for a payment received and for processing.
- A wallet inside the server: SatBooth holds no spend key.
- Lightning on LND or Core Lightning: SatBooth's Lightning is its own Alby Hub.

## 7. The cut-over

1. **Run both.** Set up SatBooth and test it with small payments on each rail you will use, while BTCPay keeps taking
   payments.
2. **Switch new orders to SatBooth.** Deploy your changed application, so that every new order is created with
   `POST /v1/orders` and every customer goes to a `checkout_url`. If your old and your new application are two
   programs, give SatBooth both as webhook endpoints while they overlap: `satbooth-setup` writes one endpoint, named
   `main`, for the receiver you give it. Add the other as a second entry of `webhooks` in `satbooth.json`, with a secret
   file of its own (`deploy/README.md`, "Adding a webhook endpoint"). Every endpoint gets every event, so run both
   until the orders the old application made are done, then remove its entry and restart SatBooth. A new endpoint is
   sent the last 24 hours of events when it starts, so it must ignore the orders it does not know (look the order up,
   and skip the ones that are not yours).
3. **Let BTCPay's open invoices end.** Invoices BTCPay already issued expire after their `expirationMinutes` (the store
   default is 15), and BTCPay then keeps monitoring them for `monitoringMinutes` (the store default is 1440, one day),
   so a payment sent late can still arrive (BTCPay Server v2.4.7, `swagger.template.invoices.json`, `CheckoutOptions`).
   Wait for the longest of these times your store uses, and settle or refund what is left, before you stop BTCPay.
   Your webhook receiver needs to handle BTCPay's events until then.
4. **Keep BTCPay for its history.** Do not send customers to it; use it to read old invoices and to refund old
   payments from your own wallet.
5. **Then stop it.** Export what you need first. Close or drain the old node's channels (section 4) and stop BTCPay
   once nothing in it is still needed.
