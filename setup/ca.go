package setup

import (
	"bytes"
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
// service mounts, and is never copied; the stub's two files and the CA's certificate are in config/, HUB_CONFIG_DIR.
// setup/ also keeps its own copy of the CA's certificate (public): volume-init gives config/hub-ca.pem to the hub's
// user, so a later run of setup as the operator cannot read it, and reads the copy instead.
const (
	caCopyFile   = "setup/ca-cert.pem"
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
	st, err := l.hubState()
	if err != nil {
		return nil, err
	}
	if err := checkDir(root, "config"); err != nil {
		return nil, err
	}
	caCert, certErr := readSmall(root, caCopyFile)
	caKey, keyErr := readSmall(root, caKeyFile)
	created := false
	switch {
	case notThere(certErr) && notThere(keyErr) && !lexists(root, caCertFile):
		// A hub host set up before the bundle had a stub has no authority: make it, and the leaf below.
		if caCert, caKey, err = NewCA(rand); err != nil {
			return nil, err
		}
		// The certificate first, as stage 2 writes them: a stop between the two leaves a certificate without its key.
		for _, f := range []file{{caCertFile, caCert}, {caCopyFile, caCert}, {caKeyFile, caKey}} {
			if err := writeNew(root, f.name, f.data); err != nil {
				return nil, fmt.Errorf("setup: cannot write %s", f.name)
			}
		}
		created = true
	case notThere(certErr) && keyErr == nil:
		return nil, errors.New("setup: setup/ca-cert.pem is missing: this hub host was set up before setup kept its own copy " +
			"of the authority's certificate, and config/hub-ca.pem now belongs to the hub's user, so setup cannot read it. " +
			"Copy it once, as root: sudo cat config/hub-ca.pem > setup/ca-cert.pem (the file is public; the redirect runs as you), then run this again")
	case certErr != nil:
		return nil, errors.New("setup: cannot read setup/ca-cert.pem (it and setup/ca-key.pem must both be there, or both absent)")
	case keyErr != nil:
		return nil, errors.New("setup: cannot read setup/ca-key.pem (it and setup/ca-cert.pem must both be there, or both absent)")
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
	cfgCopy := ""
	if !created {
		cfgCopy = l.restoreConfigCA(caCert)
	}
	if err := syncDir(root, "config"); err != nil {
		return nil, err
	}
	dc := l.composeCmd("hub") + " --env-file " + st.Hub.HostDir + "/compose.env run --rm --no-deps volume-init"
	if created {
		return &Report{Next: []string{
			"This hub host had no certificate authority: made one (config/hub-ca.pem, with its key in setup/ca-key.pem) and " +
				"the stub's certificate and key (config/stub-cert.pem, config/stub-key.pem). Keep setup/ca-key.pem; no service mounts it.",
			"Add this line to " + st.Hub.HostDir + "/compose.env if it has no HUB_STUB_IP (a compose.env written before the stub " +
				"has none): HUB_STUB_IP=" + StubIP(st.Hub.ElectrumSubnet),
			"Give the new files to the hub and the stub: " + dc + ". Then start the bundle, which now includes the stub.",
		}}, nil
	}
	next := []string{
		"The stub's certificate in " + st.Hub.HostDir + "/config is renewed (2 years); the CA is unchanged.",
		"Give the new files to the stub: " + dc + ". Then restart the stub (hubstub). Neither the hub nor bitcoind restarts.",
	}
	if cfgCopy != "" {
		next = append(next, cfgCopy)
	}
	return &Report{Next: next}, nil
}

// restoreConfigCA makes config/hub-ca.pem equal to the CA's certificate when it is missing or readable and different.
// A copy the operator cannot read (the hub's user owns it) is left alone, and so is one that cannot be replaced: it
// returns the instruction to give the operator then, or "" when nothing is left to do.
func (l *Lock) restoreConfigCA(caCert []byte) string {
	root := l.root
	cur, err := readSmall(root, caCertFile)
	if err == nil && bytes.Equal(cur, caCert) {
		return ""
	}
	if err != nil && !notThere(err) {
		return ""
	}
	tmp := caCertFile + ".new"
	if lexists(root, tmp) {
		if root.Remove(tmp) != nil {
			return "config/hub-ca.pem is missing or differs from setup/ca-cert.pem and cannot be replaced: copy setup/ca-cert.pem to it, then run volume-init."
		}
	}
	if writeNew(root, tmp, caCert) == nil && root.Rename(tmp, caCertFile) == nil {
		return ""
	}
	root.Remove(tmp)
	return "config/hub-ca.pem is missing or differs from setup/ca-cert.pem and cannot be replaced: copy setup/ca-cert.pem to it, then run volume-init."
}

// hubState is the state of a hub host whose stage 2 is done and that has no unfinished run.
func (l *Lock) hubState() (*state, error) {
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
	return st, nil
}
