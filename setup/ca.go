package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"
)

// The hub trusts one certificate authority, SatBooth's own, and no system root (SSL_CERT_FILE and SSL_CERT_DIR in
// deploy/hub/compose.yml). The hub's stub serves every name below with a certificate from it, so a TLS connection to
// any other name fails verification.
const (
	stubHostName = "hubstub" // the stub's Compose service: BOLTZ_API and MEMPOOL_API name it

	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 2 * 365 * 24 * time.Hour
	// clockSkew is how far before now a certificate is already valid.
	clockSkew = 10 * time.Minute
)

// pinnedHubNames are the hosts the hub process contacts by name at its defaults (read from getAlby/hub
// v1.24.7): Alby's API and web host (channel suggestions, the update document, rates, LSP orders, OAuth), mempool.space
// (MEMPOOL_API), the two Boltz providers (swaps/provider.go:62-65), Megalith's rebalance service (REBALANCE_SERVICE_URL),
// and Alby's Electrum and VSS servers. The bundle's hub container resolves each of them to the stub.
var pinnedHubNames = []string{
	"getalby.com",
	"api.getalby.com",
	"mempool.space",
	"api.boltz.exchange",
	"api.satsrouting.exchange",
	"megalithic.me",
	"electrum.getalbypro.com",
	"vss.getalbypro.com",
}

// StubNames is the exact list of DNS names of the stub's certificate: its own first, then the pinned hosts.
func StubNames() []string {
	return append([]string{stubHostName}, pinnedHubNames...)
}

// clock is the time of certificates; tests replace it.
var clock = time.Now

// NewCA makes the certificate authority: an ECDSA P-256 key and a self-signed CA certificate, 10 years, allowed to sign
// leaves and nothing below them. It returns both as PEM.
func NewCA(rand io.Reader) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand)
	if err != nil {
		return nil, nil, errors.New("setup: cannot generate the CA key")
	}
	serial, err := newSerial(rand)
	if err != nil {
		return nil, nil, err
	}
	now := clock()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "SatBooth hub CA"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		// The authority can vouch for these names only (and their subdomains, which a DNS name constraint always
		// includes): a leaf for any other name fails verification even when it is signed with the CA key.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         StubNames(),
	}
	der, err := x509.CreateCertificate(rand, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, errors.New("setup: cannot sign the CA certificate")
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// IssueStubCert signs a new leaf for exactly StubNames with the CA, 2 years, with a fresh key. It refuses a CA key that
// does not belong to the CA certificate, and a CA certificate that has expired or is not a CA.
func IssueStubCert(caCertPEM, caKeyPEM []byte, rand io.Reader) (certPEM, keyPEM []byte, err error) {
	ca, caKey, err := loadCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	now := clock()
	if now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return nil, nil, errors.New("setup: the CA certificate is not valid now: make a new setup directory")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand)
	if err != nil {
		return nil, nil, errors.New("setup: cannot generate the stub's key")
	}
	serial, err := newSerial(rand)
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: stubHostName},
		DNSNames:              StubNames(),
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, errors.New("setup: cannot sign the stub's certificate")
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// loadCA parses the CA's certificate and key and checks that they are a pair, that the certificate is a CA and the
// key is P-256.
func loadCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, nil, errors.New("setup: the CA certificate is not a PEM certificate")
	}
	ca, err := x509.ParseCertificate(b.Bytes)
	if err != nil || !ca.IsCA {
		return nil, nil, errors.New("setup: the CA certificate is not a CA certificate")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" {
		return nil, nil, errors.New("setup: the CA key is not a PEM EC key")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil || key.Curve != elliptic.P256() {
		return nil, nil, errors.New("setup: the CA key is not a P-256 key")
	}
	pub, ok := ca.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, errors.New("setup: the CA key does not belong to the CA certificate")
	}
	return ca, key, nil
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, errors.New("setup: cannot encode a key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// newSerial is a random 127-bit positive serial number.
func newSerial(rand io.Reader) (*big.Int, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand, b[:]); err != nil {
		return nil, fmt.Errorf("setup: cannot read random bytes: %w", err)
	}
	b[0] &= 0x7f
	n := new(big.Int).SetBytes(b[:])
	if n.Sign() == 0 {
		n.SetInt64(1)
	}
	return n, nil
}

// Where the files of the CA and the stub's certificate go, in the base directory. The CA key is in setup/, which no
// service mounts, and is never copied; the other three are in config/, HUB_CONFIG_DIR.
const (
	caCertFile   = "config/hub-ca.pem"
	stubCertFile = "config/stub-cert.pem"
	stubKeyFile  = "config/stub-key.pem"
	caKeyFile    = "setup/ca-key.pem"
)

// HubRenewCert issues a new certificate and key for the stub from the CA that stage 2 made, and replaces the two files
// of config/. The CA, its key and every other file stay as they are. On a hub host set up before the bundle had a stub,
// where neither the CA's certificate nor its key exists, it makes the CA first and writes all four files. It runs on a
// hub host whose stage 2 is done, with no unfinished run. Each leaf file is written under a temporary name and renamed
// into place, the key first: a run that stops between the two leaves a pair that does not match, and running it again
// repairs that.
func (l *Lock) HubRenewCert(rand io.Reader) (*Report, error) {
	if l.closed {
		return nil, ErrClosed
	}
	root := l.root
	if err := checkDir(root, setupDir); err != nil {
		return nil, err
	}
	raw, err := readSmall(root, stateName)
	if err != nil {
		return nil, errors.New("setup: cannot read setup/state.json: run the hub's stage 2 first")
	}
	st, err := parseState(raw)
	if err != nil {
		return nil, err
	}
	if st.Host != hostHub {
		return nil, ErrNotAStage
	}
	if st.Stage != StageHubInit && st.Stage != StageHubPair {
		return nil, ErrWrongStage
	}
	if lexists(root, journalName) {
		return nil, ErrIncomplete
	}
	if err := checkDir(root, "config"); err != nil {
		return nil, err
	}
	caCert, certErr := readSmall(root, caCertFile)
	caKey, keyErr := readSmall(root, caKeyFile)
	created := false
	switch {
	case notThere(certErr) && notThere(keyErr):
		// A hub host set up before the bundle had a stub has no authority: make it, and the leaf below.
		if caCert, caKey, err = NewCA(rand); err != nil {
			return nil, err
		}
		for _, f := range []file{{caKeyFile, caKey}, {caCertFile, caCert}} {
			if err := writeNew(root, f.name, f.data); err != nil {
				return nil, fmt.Errorf("setup: cannot write %s", f.name)
			}
		}
		created = true
	case certErr != nil:
		return nil, errors.New("setup: cannot read config/hub-ca.pem (setup/ca-key.pem and it must both be there, or both absent)")
	case keyErr != nil:
		return nil, errors.New("setup: cannot read setup/ca-key.pem (it and config/hub-ca.pem must both be there, or both absent)")
	}
	cert, key, err := IssueStubCert(caCert, caKey, rand)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{stubKeyFile, key}, {stubCertFile, cert}} {
		tmp := f.name + ".new"
		if lexists(root, tmp) {
			if err := root.Remove(tmp); err != nil {
				return nil, fmt.Errorf("setup: cannot remove %s", tmp)
			}
		}
		if err := writeNew(root, tmp, f.data); err != nil {
			return nil, fmt.Errorf("setup: cannot write %s", tmp)
		}
		if err := root.Rename(tmp, f.name); err != nil {
			return nil, fmt.Errorf("setup: cannot replace %s", f.name)
		}
	}
	if err := syncDir(root, "config"); err != nil {
		return nil, err
	}
	dc := l.composeCmd("hub") + " --env-file " + st.Hub.HostDir + "/compose.env run --rm --no-deps volume-init"
	if created {
		return &Report{Next: []string{
			"This hub host had no certificate authority: made one (config/hub-ca.pem, with its key in setup/ca-key.pem) and " +
				"the stub's certificate and key (config/stub-cert.pem, config/stub-key.pem). Keep setup/ca-key.pem; no service mounts it.",
			"Give the new files to the hub and the stub: " + dc + ". Then start the bundle, which now includes the stub.",
		}}, nil
	}
	return &Report{Next: []string{
		"The stub's certificate in " + st.Hub.HostDir + "/config is renewed (2 years); the CA is unchanged.",
		"Give the new files to the stub: " + dc + ". Then restart the stub (hubstub). Neither the hub nor bitcoind restarts.",
	}}, nil
}
