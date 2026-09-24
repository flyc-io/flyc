package bootstrap

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	msg := []byte("-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----\n")
	enc, err := encrypt(key, msg)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decrypt(key, enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, msg) {
		t.Fatalf("round trip: %q != %q", dec, msg)
	}
	other := make([]byte, 32)
	_, _ = rand.Read(other)
	if _, err := decrypt(other, enc); err == nil {
		t.Fatal("déchiffrement avec une autre clé devrait échouer")
	}
}

func TestHashPasswordFormat(t *testing.T) {
	h := HashPassword("secret")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("format PHC inattendu: %s", h)
	}
	if h == HashPassword("secret") {
		t.Fatal("deux hachages du même mot de passe doivent différer (sel)")
	}
}
