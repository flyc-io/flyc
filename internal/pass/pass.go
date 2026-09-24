// Package pass : émission et vérification des pass Flyc (JWT ES256).
// Le même code sert à flyc-queue (émission à l'admission) et aux outils d'exploitation.
package pass

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Claims du pass. Voir le plan directeur, section « Le pass ».
type Claims struct {
	Sub    string        // identifiant du ticket admis
	Tenant string        // tid
	Dom    string        // domaine protégé, comparé au Host par HAProxy
	Room   string        // rm
	TTL    time.Duration // durée de validité (exp = now + TTL)
}

// Payload : claims d'un pass vérifié.
type Payload struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
	Tid string `json:"tid,omitempty"`
	Dom string `json:"dom"`
	Rm  string `json:"rm"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Kid string `json:"kid"`
}

// ParsePrivate lit une clé privée EC P-256 en PEM.
func ParsePrivate(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("clé privée : PEM invalide")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// ParsePublic lit une clé publique PKIX en PEM.
func ParsePublic(pemStr string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("clé publique : PEM invalide")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("clé publique : pas une clé ECDSA")
	}
	return pub, nil
}

// Mint signe un pass ES256. Les claims sont figés côté serveur, jamais fournis par le client.
func Mint(privatePEM, kid string, c Claims) (string, error) {
	priv, err := ParsePrivate(privatePEM)
	if err != nil {
		return "", err
	}
	if c.Sub == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		c.Sub = "t_" + hex.EncodeToString(b)
	}
	if c.TTL == 0 {
		c.TTL = 20 * time.Minute
	}
	now := time.Now().Unix()
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	pl, _ := json.Marshal(Payload{Iss: "flyc", Sub: c.Sub, Tid: c.Tenant, Dom: strings.ToLower(c.Dom), Rm: c.Room, Iat: now, Exp: now + int64(c.TTL.Seconds()), Kid: kid})
	signing := b64(hdr) + "." + b64(pl)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		return "", err
	}
	sig := append(pad32(r), pad32(s)...) // JWS : R||S brut, pas DER
	return signing + "." + b64(sig), nil
}

// Verify vérifie signature, expiration et domaine. Renvoie les claims.
func Verify(publicPEM, token, dom string) (*Payload, error) {
	pub, err := ParsePublic(publicPEM)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("format JWT invalide")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("signature invalide")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("signature refusée")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Exp <= time.Now().Unix() {
		return nil, errors.New("pass expiré")
	}
	if dom != "" && p.Dom != strings.ToLower(dom) {
		return nil, fmt.Errorf("pass émis pour %s, pas pour %s", p.Dom, dom)
	}
	return &p, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(i *big.Int) []byte {
	out := make([]byte, 32)
	i.FillBytes(out)
	return out
}
