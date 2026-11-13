// Command satbooth-approve is the signing tool for refund and void approvals. A merchant's admin system runs
// it (or does the same in its own code) with an Ed25519 key that never touches the SatBooth server: SatBooth
// holds only the public key and checks the approvals. The key file holds the 32-byte seed as 64 lowercase hex
// digits and an optional newline, mode 0600. No flag takes key material, and no error prints the key or
// any part of the file.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/satbooth/satbooth/approval"
)

const (
	minExpiresIn = time.Minute
	// maxExpiresIn is an hour under approval.MaxLifetime (720h), so an approval made at the cap still passes on a
	// server whose clock is behind the admin system's by less than an hour.
	maxExpiresIn = 719 * time.Hour
)

var (
	errKeyFile   = errors.New("key file must be a regular file with mode 0600 or stricter, not a symlink")
	errKeyFormat = errors.New("key file must hold exactly 64 lowercase hex digits and at most one newline")
	errKeyIO     = errors.New("cannot read the key file")
	errKeyBad    = errors.New("the key is not usable")
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now, rand.Reader))
}

// run returns the exit code: 0 on success, 2 for a usage error, 1 for any other failure. Messages are fixed
// text; flag values are never echoed.
func run(args []string, stdout, stderr io.Writer, now func() time.Time, rnd io.Reader) int {
	fail := func(code int, msg string) int {
		fmt.Fprintln(stderr, "satbooth-approve:", msg)
		if code == 2 {
			fmt.Fprintln(stderr, "usage: satbooth-approve keygen -out <path>")
			fmt.Fprintln(stderr, "       satbooth-approve refund -key <path> -network <name> -order-id <hex32> -amount-msat <n> -expires-in <dur>")
			fmt.Fprintln(stderr, "       satbooth-approve void -key <path> -network <name> -refund-id <hex32> "+
				"-expires-in <dur>   (ends an open refund; one the hub reports pending waits)")
		}
		return code
	}
	if len(args) < 1 {
		return fail(2, "a command is required")
	}
	fs := flag.NewFlagSet("satbooth-approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var out, keyPath, network, orderID, refundID, expiresIn, amountText string
	switch args[0] {
	case "keygen":
		fs.StringVar(&out, "out", "", "")
	case "refund":
		fs.StringVar(&keyPath, "key", "", "")
		fs.StringVar(&network, "network", "", "")
		fs.StringVar(&orderID, "order-id", "", "")
		fs.StringVar(&amountText, "amount-msat", "", "")
		fs.StringVar(&expiresIn, "expires-in", "", "")
	case "void":
		fs.StringVar(&keyPath, "key", "", "")
		fs.StringVar(&network, "network", "", "")
		fs.StringVar(&refundID, "refund-id", "", "")
		fs.StringVar(&expiresIn, "expires-in", "", "")
	default:
		return fail(2, "unknown command")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return fail(2, "invalid flags")
	}
	if fs.NArg() != 0 {
		return fail(2, "unexpected argument")
	}

	if args[0] == "keygen" {
		if out == "" {
			return fail(2, "-out is required")
		}
		pub, err := keygen(out, rnd)
		if err != nil {
			return fail(1, err.Error())
		}
		fmt.Fprintln(stdout, pub)
		return 0
	}

	if keyPath == "" {
		return fail(2, "-key is required")
	}
	net, err := approval.ParseNetwork(network)
	if err != nil {
		return fail(2, "-network must be mainnet, testnet, signet or regtest")
	}
	d, err := time.ParseDuration(expiresIn)
	if err != nil || d < minExpiresIn || d > maxExpiresIn {
		return fail(2, "-expires-in must be a duration from 1m to 719h")
	}
	expiresAt := now().Unix() + int64(d/time.Second)

	switch args[0] {
	case "refund":
		oid, ok := parseID(orderID)
		if !ok {
			return fail(2, "-order-id must be 32 lowercase hex digits, not all zero")
		}
		amount, ok := parseAmount(amountText)
		if !ok || amount%1000 != 0 || amount > approval.MaxAmountMsat {
			return fail(2, "-amount-msat must be a positive multiple of 1000, at most 2^53")
		}
		priv, err := loadKey(keyPath)
		if err != nil {
			return fail(1, err.Error())
		}
		defer clear(priv)
		id := approval.KeyID(priv.Public().(ed25519.PublicKey))
		var rid [16]byte
		for rid == ([16]byte{}) {
			if _, err := io.ReadFull(rnd, rid[:]); err != nil {
				return fail(1, "cannot read random bytes")
			}
		}
		s := approval.SignRefund(approval.Refund{Network: net, KeyID: id, RefundID: rid, OrderID: oid,
			AmountMsat: uint64(amount), ExpiresAt: expiresAt}, priv)
		fmt.Fprintf(stdout, "refund_id=%s\napproval=%s\n", hex.EncodeToString(rid[:]), s)
	case "void":
		rid, ok := parseID(refundID)
		if !ok {
			return fail(2, "-refund-id must be 32 lowercase hex digits, not all zero")
		}
		priv, err := loadKey(keyPath)
		if err != nil {
			return fail(1, err.Error())
		}
		defer clear(priv)
		id := approval.KeyID(priv.Public().(ed25519.PublicKey))
		s := approval.SignVoid(approval.Void{Network: net, KeyID: id, RefundID: rid, ExpiresAt: expiresAt}, priv)
		fmt.Fprintf(stdout, "approval=%s\n", s)
	}
	return 0
}

// keygen makes a new key from rnd and writes its seed to a new file (O_EXCL, mode 0600). It returns the public key.
func keygen(path string, rnd io.Reader) (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	defer clear(seed)
	if _, err := io.ReadFull(rnd, seed); err != nil {
		return "", errors.New("cannot read random bytes")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	defer clear(priv)
	pub := priv.Public().(ed25519.PublicKey)
	if _, err := approval.NewKeys([]string{hex.EncodeToString(pub)}); err != nil {
		return "", errKeyBad
	}
	line := append([]byte(hex.EncodeToString(seed)), '\n')
	defer clear(line)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", errors.New("cannot create the key file (it must not exist yet)")
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(path)
		return "", errors.New("cannot write the key file")
	}
	return hex.EncodeToString(pub), nil
}

// loadKey reads the key file strictly, as nostr.LoadSecretKeyFile does: a regular file, not a symlink, no
// group or other permission bit, exactly 64 lowercase hex digits and at most one newline.
func loadKey(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, errKeyIO
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errKeyFile
	}
	// O_NOFOLLOW refuses a symlink swapped in after Lstat; O_NONBLOCK keeps a swapped-in FIFO from hanging the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errKeyIO
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil {
		return nil, errKeyIO
	}
	if !fi2.Mode().IsRegular() || fi2.Mode().Perm()&0o077 != 0 || !os.SameFile(fi, fi2) {
		return nil, errKeyFile
	}
	buf := make([]byte, 64+1+1) // 64 digits, a newline, and one byte to detect anything longer
	defer clear(buf)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, errKeyIO
	}
	data := buf[:n]
	if len(data) == 65 && data[64] == '\n' {
		data = data[:64]
	}
	if len(data) != 64 {
		return nil, errKeyFormat
	}
	seed, ok := decodeHex(string(data))
	defer clear(seed)
	if !ok {
		return nil, errKeyFormat
	}
	priv := ed25519.NewKeyFromSeed(seed)
	// The public key must be one the server's NewKeys accepts.
	if _, err := approval.NewKeys([]string{hex.EncodeToString(priv.Public().(ed25519.PublicKey))}); err != nil {
		clear(priv)
		return nil, errKeyBad
	}
	return priv, nil
}

// parseID decodes 32 lowercase hex digits that are not all zero.
func parseID(s string) ([16]byte, bool) {
	var id [16]byte
	if len(s) != 32 {
		return id, false
	}
	b, ok := decodeHex(s)
	if !ok {
		return id, false
	}
	copy(id[:], b)
	return id, id != [16]byte{}
}

// decodeHex decodes lowercase hex only (encoding/hex alone also accepts uppercase).
func decodeHex(s string) ([]byte, bool) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return out, true
}

// parseAmount reads -amount-msat as plain decimal only: 1 to 16 digits, the first not 0; no sign, base prefix,
// underscore or space. Anything else is refused (flag's own integer parsing would read 0x10 or 017500 as numbers).
func parseAmount(s string) (int64, bool) {
	if len(s) < 1 || len(s) > 16 || s[0] == '0' {
		return 0, false
	}
	var v int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + int64(s[i]-'0')
	}
	return v, true
}
