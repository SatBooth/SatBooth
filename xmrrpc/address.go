package xmrrpc

// The Monero networks wallet-rpc can serve. A regtest wallet uses mainnet's address prefixes.
const (
	Mainnet  = "mainnet"
	Testnet  = "testnet"
	Stagenet = "stagenet"
	Regtest  = "regtest"
)

// addressLen is the length of a standard Monero address or subaddress in Monero's base58: 69 bytes (a
// one-byte network prefix, two 32-byte keys, a 4-byte checksum) in 8-byte blocks of 11 characters, the last
// block of 5 bytes in 7.
const addressLen = 95

// base58 is Monero's base58 alphabet (no 0, O, I or l).
const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ValidNetwork reports whether s is one of the four network names.
func ValidNetwork(s string) bool {
	return s == Mainnet || s == Testnet || s == Stagenet || s == Regtest
}

// firstChars gives the leading characters of a primary address and of a subaddress per network. The first
// character follows from the network prefix (src/cryptonote_config.h): mainnet 18 and 42 give "4" and "8";
// testnet 53 spans two leading base58 digits, "9" and "A", and 63 gives "B"; stagenet 24 and 36 give "5" and
// "7".
func firstChars(network string, sub bool) string {
	switch network {
	case Mainnet, Regtest:
		if sub {
			return "8"
		}
		return "4"
	case Testnet:
		if sub {
			return "B"
		}
		return "9A"
	case Stagenet:
		if sub {
			return "7"
		}
		return "5"
	}
	return ""
}

func inBase58(c byte) bool {
	for i := 0; i < len(base58); i++ {
		if base58[i] == c {
			return true
		}
	}
	return false
}

// ValidAddress reports whether s has the shape of a primary address (sub false) or a subaddress (sub true)
// of network: 95 bytes of the base58 alphabet and the network's leading character. The address is not
// decoded: its checksum and keys are wallet-rpc's to check.
func ValidAddress(s, network string, sub bool) bool {
	firsts := firstChars(network, sub)
	if len(s) != addressLen || firsts == "" {
		return false
	}
	ok := false
	for i := 0; i < len(firsts); i++ {
		ok = ok || s[0] == firsts[i]
	}
	if !ok {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !inBase58(s[i]) {
			return false
		}
	}
	return true
}

// AnySubaddress reports whether s has the shape of a subaddress of any network ("8", "B" or "7" first).
func AnySubaddress(s string) bool {
	return ValidAddress(s, Mainnet, true) || ValidAddress(s, Testnet, true) || ValidAddress(s, Stagenet, true)
}
