// Package rail holds the rules SatBooth's watched rails share (on-chain bitcoin, and Monero): the confirmation tiers,
// what one payment of an order says (Out) and the sums and predicates over an order's payments, the steps of settle
// that do not depend on a rail's ledger or node, the transfers of a rail's two ledger accounts and the walk over a
// payment's epochs (Book), the paid events (PaidEvents), and the rail's view of an order with its state. Amounts are in
// the rail's base unit (sat or piconero); each function that sums takes its bound. It imports store, ledger and lnpay,
// never a rail.
package rail
