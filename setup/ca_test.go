package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func parseCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p)
	if b == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func verifyName(ca, leaf *x509.Certificate, name string) error {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name, CurrentTime: clock(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func newPair(t *testing.T, seed int64) (caCert, caKey, cert, key []byte) {
	t.Helper()
	r := detRand(seed)
	caCert, caKey, err := NewCA(r)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err = IssueStubCert(caCert, caKey, r)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// The chain verifies for each name of the list, and for no other.
func TestStubCertVerifiesForItsNamesOnly(t *testing.T) {
	caPEM, _, certPEM, _ := newPair(t, 1)
	ca, leaf := parseCert(t, caPEM), parseCert(t, certPEM)
	if !slices.Equal(leaf.DNSNames, StubNames()) || len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 || len(leaf.URIs) != 0 {
		t.Errorf("SANs %v %v, want exactly %v", leaf.DNSNames, leaf.IPAddresses, StubNames())
	}
	if StubNames()[0] != "hubstub" {
		t.Error("hubstub is not first")
	}
	for _, n := range StubNames() {
		if err := verifyName(ca, leaf, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, n := range []string{"example.com", "www.getalby.com", "evil.getalby.com", "hub", "stub", "localhost",
		"blockstream.info", "relay.getalby.com", "megalithic.me.evil.test"} {
		if err := verifyName(ca, leaf, n); err == nil {
			t.Errorf("verification passed for %q", n)
		}
	}
	// A system-style trust store without our CA does not verify it, and the leaf is not a CA.
	if err := verifyName(leaf, leaf, "hubstub"); err == nil && leaf.IsCA {
		t.Error("the leaf is a CA")
	}
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("the leaf may sign")
	}
	if !ca.IsCA || ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Error("the CA may sign below a leaf")
	}
	if ca.NotAfter.Sub(ca.NotBefore) < 10*365*24*time.Hour || leaf.NotAfter.Sub(leaf.NotBefore) < 2*365*24*time.Hour ||
		leaf.NotAfter.Sub(leaf.NotBefore) > 2*365*24*time.Hour+time.Hour {
		t.Errorf("lifetimes: CA %v, leaf %v", ca.NotAfter.Sub(ca.NotBefore), leaf.NotAfter.Sub(leaf.NotBefore))
	}
}

// Seeded property: for random subsets and mutations of the names, a name verifies exactly when it is in the list.
func TestStubCertSANProperty(t *testing.T) {
	caPEM, _, certPEM, _ := newPair(t, 2)
	ca, leaf := parseCert(t, caPEM), parseCert(t, certPEM)
	if !reflect.DeepEqual(leaf.DNSNames, StubNames()) {
		t.Fatal("SAN list does not round-trip")
	}
	r := rand.New(rand.NewSource(7))
	names := StubNames()
	alphabet := "abcdefghijklmnopqrstuvwxyz0123456789-.*_ABC"
	for i := 0; i < 2000; i++ {
		var n string
		switch r.Intn(4) {
		case 0:
			n = names[r.Intn(len(names))]
		case 1: // a listed name with one character changed, added or dropped
			b := []byte(names[r.Intn(len(names))])
			switch p := r.Intn(len(b)); r.Intn(3) {
			case 0:
				b[p] = alphabet[r.Intn(len(alphabet))]
			case 1:
				b = append(b[:p], append([]byte{alphabet[r.Intn(len(alphabet))]}, b[p:]...)...)
			default:
				b = append(b[:p], b[p+1:]...)
			}
			n = string(b)
		case 2: // a listed name with a label added or its case changed
			n = []string{"x.", "www.", "a.b."}[r.Intn(3)] + names[r.Intn(len(names))]
		default:
			b := make([]byte, r.Intn(30))
			for j := range b {
				b[j] = alphabet[r.Intn(len(alphabet))]
			}
			n = string(b)
		}
		if n == "" {
			continue // Go checks no name for an empty one
		}
		in := slices.ContainsFunc(names, func(s string) bool { return strings.EqualFold(s, strings.TrimSuffix(n, ".")) })
		if err := verifyName(ca, leaf, n); (err == nil) != in {
			t.Fatalf("%q: in list %v, verify error %v", n, in, err)
		}
	}
}

// A leaf from another CA does not verify under ours.
func TestStubCertFromAnotherCAFails(t *testing.T) {
	caA, _, _, _ := newPair(t, 3)
	_, _, certB, _ := newPair(t, 4)
	if err := verifyName(parseCert(t, caA), parseCert(t, certB), "hubstub"); err == nil {
		t.Error("a leaf of another CA verifies")
	}
}

func TestIssueRefusesBadCA(t *testing.T) {
	caCert, caKey, cert, key := newPair(t, 5)
	_, otherKey, _, _ := newPair(t, 6)
	for name, c := range map[string]struct{ cert, key []byte }{
		"key of another CA":  {caCert, otherKey},
		"a leaf as the CA":   {cert, key},
		"garbage":            {[]byte("x"), caKey},
		"no key":             {caCert, nil},
		"certificate as key": {caCert, caCert},
	} {
		if _, _, err := IssueStubCert(c.cert, c.key, detRand(1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	old := clock
	defer func() { clock = old }()
	clock = func() time.Time { return old().Add(11 * 365 * 24 * time.Hour) }
	if _, _, err := IssueStubCert(caCert, caKey, detRand(1)); err == nil {
		t.Error("an expired CA signed a leaf")
	}
}

// The leaf never outlives the CA.
func TestLeafEndsWithTheCA(t *testing.T) {
	old := clock
	defer func() { clock = old }()
	caCert, caKey, _, _ := newPair(t, 8)
	ca := parseCert(t, caCert)
	clock = func() time.Time { return ca.NotAfter.Add(-24 * time.Hour) }
	c, _, err := IssueStubCert(caCert, caKey, detRand(2))
	if err != nil {
		t.Fatal(err)
	}
	if parseCert(t, c).NotAfter.After(ca.NotAfter) {
		t.Error("the leaf outlives the CA")
	}
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// Stage 2 writes the four files, every one 0600, the CA key only under setup/, and no file of config/ or of the
// state holds the CA key.
func TestHubInitWritesTheCA(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(2)
	d := e.hubDir
	for _, n := range []string{caCertFile, caCopyFile, stubCertFile, stubKeyFile, caKeyFile} {
		if m := fileMode(t, filepath.Join(d, n)); m != 0o600 {
			t.Errorf("%s has mode %o", n, m)
		}
	}
	if string(e.read(d, caCopyFile)) != string(e.read(d, caCertFile)) {
		t.Error("setup/ca-cert.pem is not a copy of config/hub-ca.pem")
	}
	caKey := e.read(d, caKeyFile)
	if !strings.Contains(string(caKey), "EC PRIVATE KEY") {
		t.Fatal("setup/ca-key.pem is not a key")
	}
	err := filepath.WalkDir(d, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(d, p)
		b, _ := os.ReadFile(p)
		if rel != caKeyFile && strings.Contains(string(b), strings.TrimSpace(string(caKey[strings.Index(string(caKey), "\n"):]))[:40]) {
			t.Errorf("the CA key's content is in %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ca, leaf := parseCert(t, e.read(d, caCertFile)), parseCert(t, e.read(d, stubCertFile))
	for _, n := range StubNames() {
		if err := verifyName(ca, leaf, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if strings.Contains(string(e.read(d, stubKeyFile)), string(caKey)) || string(e.read(d, stubKeyFile)) == string(caKey) {
		t.Error("the stub's key is the CA's")
	}
}

func renew(t *testing.T, dir string, seed int64) (*Report, error) {
	t.Helper()
	lk, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Close()
	return lk.HubRenewCert(detRand(seed))
}

// A rerun renews the leaf and keeps the CA, its key and every other file, at the same modes.
func TestRenewKeepsTheCA(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	before := snapshot(t, d)
	old := clock
	defer func() { clock = old }()
	clock = func() time.Time { return old().Add(400 * 24 * time.Hour) }
	if _, err := renew(t, d, 9); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, d)
	for n, v := range before {
		switch n {
		case stubCertFile, stubKeyFile:
			if after[n] == v {
				t.Errorf("%s was not renewed", n)
			}
		default:
			if after[n] != v {
				t.Errorf("%s changed", n)
			}
		}
	}
	if len(after) != len(before) {
		t.Errorf("files: %d before, %d after (a temporary file is left?)", len(before), len(after))
	}
	for _, n := range []string{stubCertFile, stubKeyFile} {
		if m := fileMode(t, filepath.Join(d, n)); m != 0o600 {
			t.Errorf("%s has mode %o", n, m)
		}
	}
	ca, leaf := parseCert(t, e.read(d, caCertFile)), parseCert(t, e.read(d, stubCertFile))
	for _, n := range StubNames() {
		if err := verifyName(ca, leaf, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if !leaf.NotBefore.After(old()) {
		t.Error("the renewed leaf is not newer")
	}
	// A second rerun works too, and leaves no temporary file.
	if _, err := renew(t, d, 10); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(d, stubCertFile+".new")) || exists(filepath.Join(d, stubKeyFile+".new")) {
		t.Error("a temporary file is left")
	}
}

func TestRenewRefusals(t *testing.T) {
	// Not set up at all.
	if _, err := renew(t, baseDir(t), 1); err == nil {
		t.Error("renewed in an empty directory")
	}
	// The public host's directory is not a hub host.
	e := newEnv(t, fxEveryRail(t))
	e.upTo(1)
	if _, err := renew(t, e.pubDir, 1); err == nil {
		t.Error("renewed on the public host")
	}
	// A CA key that is gone or does not match the CA certificate.
	e = newEnv(t, fxEveryRail(t))
	e.upTo(2)
	other := newEnv(t, fxEveryRail(t))
	other.upTo(2)
	if err := os.WriteFile(filepath.Join(e.hubDir, caKeyFile), other.read(other.hubDir, caKeyFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(t, e.hubDir, 1); err == nil {
		t.Error("renewed with another CA's key")
	}
	if err := os.Remove(filepath.Join(e.hubDir, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(t, e.hubDir, 1); err == nil {
		t.Error("renewed without the CA key")
	}
}

// After volume-init the hub's user owns config/hub-ca.pem and the operator cannot read it: a renewal reads setup's own
// copy and succeeds, and leaves the unreadable file alone.
func TestRenewWithTheConfigCopyUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every file")
	}
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	p := filepath.Join(d, caCertFile)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	if _, err := renew(t, d, 9); err != nil {
		t.Fatalf("renew with config/hub-ca.pem unreadable: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	ca, leaf := parseCert(t, e.read(d, caCopyFile)), parseCert(t, e.read(d, stubCertFile))
	for _, n := range StubNames() {
		if err := verifyName(ca, leaf, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	// A config copy that is missing is restored from setup's.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(t, d, 10); err != nil {
		t.Fatal(err)
	}
	if string(e.read(d, caCertFile)) != string(e.read(d, caCopyFile)) {
		t.Error("config/hub-ca.pem was not restored")
	}
}

// A host set up before setup kept its own copy has the key and not the copy: the error names that cause and the fix.
func TestRenewWithoutSetupsCopy(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	if err := os.Remove(filepath.Join(d, caCopyFile)); err != nil {
		t.Fatal(err)
	}
	_, err := renew(t, d, 9)
	if err == nil || !strings.Contains(err.Error(), "setup/ca-cert.pem is missing") || !strings.Contains(err.Error(), "config/hub-ca.pem > setup/ca-cert.pem") {
		t.Fatalf("error: %v", err)
	}
}

// A leftover config/hub-ca.pem with neither setup file is named as such, not reported as an unreadable pair.
func TestRenewWithOnlyTheConfigCopy(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	for _, n := range []string{caCopyFile, caKeyFile} {
		if err := os.Remove(filepath.Join(d, n)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := renew(t, d, 9)
	if err == nil || !strings.Contains(err.Error(), "only config/hub-ca.pem exists") || !strings.Contains(err.Error(), "delete config/hub-ca.pem") {
		t.Fatalf("error: %v", err)
	}
}

// A stop after the second write (setup's copy present, the key absent) names both files to delete.
func TestRenewWithoutTheKey(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	if err := os.Remove(filepath.Join(d, caKeyFile)); err != nil {
		t.Fatal(err)
	}
	_, err := renew(t, d, 9)
	if err == nil || !strings.Contains(err.Error(), "setup/ca-key.pem is missing") ||
		!strings.Contains(err.Error(), "delete setup/ca-cert.pem and config/hub-ca.pem") {
		t.Fatalf("error: %v", err)
	}
}

// The CA itself refuses to vouch for any other name: a leaf made with its key for a name outside the list fails
// verification, alone or beside listed names, and every listed name still verifies.
func TestCAIsConstrainedToTheNames(t *testing.T) {
	caPEM, keyPEM, goodPEM, _ := newPair(t, 12)
	ca := parseCert(t, caPEM)
	_, caKey, err := loadCA(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(ca.PermittedDNSDomains) != len(StubNames()) || !ca.PermittedDNSDomainsCritical {
		t.Fatalf("constraints: %v critical %v", ca.PermittedDNSDomains, ca.PermittedDNSDomainsCritical)
	}
	for i, n := range StubNames() {
		if ca.PermittedDNSDomains[i] != n {
			t.Errorf("permitted[%d] = %q, want %q", i, ca.PermittedDNSDomains[i], n)
		}
	}
	for _, n := range StubNames() {
		if err := verifyName(ca, parseCert(t, goodPEM), n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	rogue := func(names ...string) *x509.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), detRand(3))
		tpl := &x509.Certificate{SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
			NotBefore: clock().Add(-time.Hour), NotAfter: clock().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(detRand(4), tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, names := range [][]string{{"evil.example"}, {"blockstream.info"}, {"relay.example.org"}, {"hubstub", "evil.example"}} {
		leaf := rogue(names...)
		for _, n := range names {
			if err := verifyName(ca, leaf, n); err == nil {
				t.Errorf("a leaf for %v verified for %q", names, n)
			}
		}
	}
	if err := verifyName(ca, rogue("getalby.com"), "getalby.com"); err != nil {
		t.Errorf("a leaf for a listed name: %v", err)
	}
}

// A hub host set up before the bundle had a stub has no authority: renew-cert makes it and the leaf, 0600, with a
// chain that verifies; the next run keeps that authority. One of the two authority files alone is refused.
func TestRenewMakesTheCAOfAnOlderHost(t *testing.T) {
	e := newEnv(t, fxEveryRail(t))
	e.upTo(3)
	d := e.hubDir
	for _, n := range []string{caCertFile, caCopyFile, caKeyFile, stubCertFile, stubKeyFile} {
		if err := os.Remove(filepath.Join(d, n)); err != nil {
			t.Fatal(err)
		}
	}
	// One of the pair alone: refused, nothing written.
	if err := os.WriteFile(filepath.Join(d, caKeyFile), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := renew(t, d, 1); err == nil {
		t.Fatal("renewed with a CA key and no CA certificate")
	}
	if exists(filepath.Join(d, stubCertFile)) {
		t.Fatal("a refused run wrote a file")
	}
	os.Remove(filepath.Join(d, caKeyFile))

	rep, err := renew(t, d, 2)
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(rep.Next, "\n"); !strings.Contains(text, "had no certificate authority") || !strings.Contains(text, "volume-init") ||
		!strings.Contains(text, "HUB_STUB_IP="+StubIP(e.hub.ElectrumSubnet)) {
		t.Errorf("report: %s", text)
	}
	for _, n := range []string{caCertFile, caCopyFile, caKeyFile, stubCertFile, stubKeyFile} {
		if m := fileMode(t, filepath.Join(d, n)); m != 0o600 {
			t.Errorf("%s has mode %o", n, m)
		}
	}
	ca, leaf := parseCert(t, e.read(d, caCertFile)), parseCert(t, e.read(d, stubCertFile))
	for _, n := range StubNames() {
		if err := verifyName(ca, leaf, n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	caBefore := string(e.read(d, caCertFile))
	rep, err = renew(t, d, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.read(d, caCertFile)) != caBefore || !strings.Contains(strings.Join(rep.Next, "\n"), "the CA is unchanged") {
		t.Error("the second run did not keep the CA")
	}
}
