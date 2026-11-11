package integration

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGenCertsScript runs lab/gen-certs.sh and checks that each server
// certificate loads as a key pair and verifies against the generated CA for
// its own hostname only.
func TestGenCertsScript(t *testing.T) {
	for _, bin := range []string{"bash", "openssl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()
	script, err := filepath.Abs("../../lab/gen-certs.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		out, err := exec.Command("bash", script, dir).CombinedOutput()
		if err != nil {
			t.Fatalf("gen-certs.sh: %v\n%s", err, out)
		}
	}
	run()
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	// A second run must keep the CA, so certificates trusted before stay trusted.
	run()
	caPEM2, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(caPEM) != string(caPEM2) {
		t.Fatal("second run replaced the CA")
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt holds no certificate")
	}
	hosts := []string{"a.test", "b.test"}
	for _, host := range hosts {
		pair, err := tls.LoadX509KeyPair(filepath.Join(dir, host+".crt"), filepath.Join(dir, host+".key"))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		leaf := pair.Leaf
		if leaf == nil {
			t.Fatalf("%s: no parsed leaf", host)
		}
		for _, name := range hosts {
			_, err := leaf.Verify(x509.VerifyOptions{
				DNSName:   name,
				Roots:     roots,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if name == host && err != nil {
				t.Errorf("%s certificate does not verify for %s: %v", host, name, err)
			}
			if name != host && err == nil {
				t.Errorf("%s certificate also verifies for %s", host, name)
			}
		}
		info, err := os.Stat(filepath.Join(dir, host+".key"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s.key mode %v is readable by others", host, perm)
		}
	}
}
