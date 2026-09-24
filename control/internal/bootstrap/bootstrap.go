// Package bootstrap : initialisation au premier démarrage de flyc-control.
//   - paire de clés ES256 de signature des pass (privée chiffrée avec la clé maître)
//   - premier administrateur plateforme, mot de passe écrit une seule fois sur disque
package bootstrap

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

// Result expose ce dont le serveur a besoin après bootstrap.
type Result struct {
	KID        string
	PrivatePEM string
	PublicPEM  string
	JWKS       map[string]any
}

// Run est idempotent : ne crée que ce qui manque.
func Run(ctx context.Context, pool *pgxpool.Pool, masterKeyB64, dataDir string, log *slog.Logger) (*Result, error) {
	masterKey, err := base64.StdEncoding.DecodeString(masterKeyB64)
	if err != nil || len(masterKey) != 32 {
		return nil, errors.New("FLYC_MASTER_KEY doit être 32 octets en base64")
	}

	res, err := ensureSigningKey(ctx, pool, masterKey, log)
	if err != nil {
		return nil, err
	}
	if err := ensureAdmin(ctx, pool, masterKey, dataDir, log); err != nil {
		return nil, err
	}
	if err := ensurePlatformKey(ctx, pool, dataDir, log); err != nil {
		return nil, err
	}
	return res, nil
}

// ensurePlatformKey crée la première clé API plateforme et l'écrit une seule fois sur disque.
func ensurePlatformKey(ctx context.Context, pool *pgxpool.Pool, dataDir string, log *slog.Logger) error {
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from api_keys where tenant_id is null`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	raw := make([]byte, 20)
	_, _ = rand.Read(raw)
	key := "flyc_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(dataDir, "initial-platform-api-key.txt")
	content := fmt.Sprintf("Clé API plateforme flyc-control (accès total)\n%s\n\nUsage : Authorization: Bearer <clé> ; ajouter X-Flyc-Tenant: <slug> pour agir sur un tenant.\nSupprimez ce fichier après l'avoir rangé.\n", key)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("écriture de la clé API initiale dans %s: %w", path, err)
	}
	if _, err := pool.Exec(ctx, `insert into api_keys(tenant_id, name, key_hash) values(null, 'initial-platform', $1)`, hex.EncodeToString(sum[:])); err != nil {
		_ = os.Remove(path)
		return err
	}
	log.Warn("clé API plateforme créée", "key_file", path)
	return nil
}

func ensureSigningKey(ctx context.Context, pool *pgxpool.Pool, masterKey []byte, log *slog.Logger) (*Result, error) {
	var kid string
	var encPriv []byte
	var pubPEM string
	err := pool.QueryRow(ctx, `select kid, private_pem, public_pem from signing_keys where active order by created_at desc limit 1`).Scan(&kid, &encPriv, &pubPEM)
	switch {
	case err == nil:
		privPEM, err := decrypt(masterKey, encPriv)
		if err != nil {
			return nil, fmt.Errorf("déchiffrement de la clé de signature %s (clé maître changée ?): %w", kid, err)
		}
		return build(kid, string(privPEM), pubPEM)
	case errors.Is(err, pgx.ErrNoRows):
		// génération
	default:
		return nil, err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	kid = time.Now().UTC().Format("2006-01") + "-" + randHex(3)

	enc, err := encrypt(masterKey, privPEM)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `insert into signing_keys(kid, private_pem, public_pem, active) values($1,$2,$3,true)`, kid, enc, pubPEM); err != nil {
		return nil, err
	}
	log.Info("clé de signature des pass générée", "kid", kid)
	return build(kid, string(privPEM), pubPEM)
}

// RotateSigningKey génère une nouvelle clé active et retire l'ancienne (acceptée encore 24 h).
func RotateSigningKey(ctx context.Context, pool *pgxpool.Pool, masterKey []byte) (*Result, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	privDER, _ := x509.MarshalECPrivateKey(priv)
	pubDER, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	kid := time.Now().UTC().Format("2006-01") + "-" + randHex(3)
	enc, err := encrypt(masterKey, privPEM)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update signing_keys set active=false, retired_at=now() where active`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `insert into signing_keys(kid, private_pem, public_pem, active) values($1,$2,$3,true)`, kid, enc, pubPEM); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return build(kid, string(privPEM), pubPEM)
}

// JWK : représentation JWK d'une clé publique PEM.
func JWK(kid, pubPEM string) (map[string]any, error) {
	r, err := build(kid, "", pubPEM)
	if err != nil {
		return nil, err
	}
	return r.JWKS["keys"].([]any)[0].(map[string]any), nil
}

func build(kid, privPEM, pubPEM string) (*Result, error) {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return nil, errors.New("clé publique PEM invalide")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("clé publique : pas une clé ECDSA")
	}
	jwk := map[string]any{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(padTo32(pub.X)),
		"y": base64.RawURLEncoding.EncodeToString(padTo32(pub.Y)),
	}
	return &Result{KID: kid, PrivatePEM: privPEM, PublicPEM: pubPEM, JWKS: map[string]any{"keys": []any{jwk}}}, nil
}

func padTo32(b *big.Int) []byte {
	out := make([]byte, 32)
	b.FillBytes(out)
	return out
}

func ensureAdmin(ctx context.Context, pool *pgxpool.Pool, masterKey []byte, dataDir string, log *slog.Logger) error {
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	password := randHex(12)
	hash := HashPassword(password)

	// Le fichier est écrit avant l'insertion : si le disque refuse, aucun compte
	// n'est créé et le prochain démarrage réessaiera.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("répertoire de données %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, "initial-admin-password.txt")
	content := fmt.Sprintf("Premier administrateur flyc-control\nemail    : admin@localhost\npassword : %s\n\nChangez ce mot de passe à la première connexion, puis supprimez ce fichier.\n", password)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("écriture du mot de passe initial dans %s (volume /data accessible en écriture par l'uid 10001 ?): %w", path, err)
	}
	if _, err := pool.Exec(ctx, `insert into users(email, password_hash, is_platform_admin) values($1,$2,true)`, "admin@localhost", hash); err != nil {
		_ = os.Remove(path)
		return err
	}
	log.Warn("premier administrateur créé", "email", "admin@localhost", "password_file", path)
	return nil
}

// HashPassword : argon2id, format PHC.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...), nil
}

func decrypt(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("données chiffrées tronquées")
	}
	return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
