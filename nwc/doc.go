// Package nwc is SatBooth's client for a Nostr Wallet Connect (NIP-47) wallet.
//
// It has two layers. The wire layer (wire.go, types.go) is pure functions that build requests and
// strictly parse responses and notifications. The client (client.go) is one WebSocket connection to one
// relay for one (client key, wallet key) pair: NIP-42 AUTH, request and response matching by the e tag,
// reconnection without resending, and notifications. The hub behaviour both rely on is pinned to
// getAlby/hub 921003b.
//
// # Outcome rule
//
// No error from a pay_invoice call proves that a payment did not happen. A response can be lost, a
// request can be dropped silently, and the hub answers INTERNAL for several states that leave a
// payment pending or settled ("there is already a payment pending for this invoice", "this invoice
// has already been paid"; getAlby/hub 921003b nip47/controllers/map_nip47_error.go maps every
// unrecognised service error to INTERNAL). Callers therefore resolve the outcome of a payment with
// LookupInvoice (by payment hash) or LookupInvoiceByString (by invoice), and treat only a settled or
// failed transaction row as an answer.
//
// A request is known never to have been sent only after a *RelayRefusedError, an argument-validation
// error, ErrClosed from a call made after Close, or a context error that does not wrap ErrNoResponse
// (the context was done before the request was registered). Everything else is outcome unknown.
//
// # Notifications
//
// Within one Client each notification is delivered once; across a process restart delivery is
// at-least-once (the subscription has no "since", so the relay replays its window). Treat a notification
// as a hint and match it by payment_hash.
//
// # Strictness
//
// Everything a wallet sends is untrusted. Responses and notifications are parsed with exact key
// matching, no duplicate keys, valid UTF-8, bounded nesting, integers in 0 to 2^53 only, and a
// validated Transaction. Keys that are unknown inside "result" are ignored; an unknown key at the top
// level of the envelope is ErrMalformed. Error values never contain wallet-supplied text beyond a
// quoted, truncated message.
//
// BuildRequest and OpenResponse are exported for the qualification suite. They are diagnostic, not for
// SatBooth's own code paths, which cache the conversation key and use the unexported variants.
package nwc
