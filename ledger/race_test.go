//go:build race

package ledger

// raceOn is true under the race detector, which makes the long property runs several times slower.
const raceOn = true
