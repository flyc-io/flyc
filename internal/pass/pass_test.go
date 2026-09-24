package pass

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func keys(t *testing.T) (string, string) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pd, _ := x509.MarshalECPrivateKey(priv)
	pub, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pd})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))
}

func TestMintVerify(t *testing.T) {
	priv, pub := keys(t)
	tok, err := Mint(priv, "k1", Claims{Dom: "Billets.Example.com", Room: "demo.concert", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Verify(pub, tok, "billets.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p.Rm != "demo.concert" || p.Dom != "billets.example.com" || p.Kid != "k1" {
		t.Fatalf("claims inattendus: %+v", p)
	}
	if _, err := Verify(pub, tok, "autre.example.com"); err == nil {
		t.Fatal("un pass pour un autre domaine doit être refusé")
	}
	_, pub2 := keys(t)
	if _, err := Verify(pub2, tok, "billets.example.com"); err == nil {
		t.Fatal("une autre clé publique doit refuser la signature")
	}
	exp, _ := Mint(priv, "k1", Claims{Dom: "billets.example.com", Room: "r", TTL: -time.Minute})
	if _, err := Verify(pub, exp, "billets.example.com"); err == nil {
		t.Fatal("un pass expiré doit être refusé")
	}
}
