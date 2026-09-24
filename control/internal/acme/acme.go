// Package acme : émission et renouvellement des certificats des domaines (HTTP-01 via les LB).
//
// Le challenge arrive sur la VIP en HTTP, HAProxy le route vers flyc-control qui sert
// /.well-known/acme-challenge/<token> depuis la table acme_challenges.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flyc-io/flyc/control/internal/crypt"
)

type Manager struct {
	pool      *pgxpool.Pool
	key       crypt.Key
	directory string
	email     string
	log       *slog.Logger
	onIssued  func()
}

func New(pool *pgxpool.Pool, key crypt.Key, directory, email string, log *slog.Logger, onIssued func()) *Manager {
	if directory == "" {
		directory = lego.LEDirectoryProduction
	}
	return &Manager{pool: pool, key: key, directory: directory, email: email, log: log, onIssued: onIssued}
}

// Enabled : un email est configuré.
func (m *Manager) Enabled() bool { return m.email != "" }

// ---- compte ACME ----

type account struct {
	email string
	key   *ecdsa.PrivateKey
	reg   *registration.Resource
}

func (a *account) GetEmail() string                        { return a.email }
func (a *account) GetRegistration() *registration.Resource { return a.reg }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

func (m *Manager) loadAccount(ctx context.Context) (*account, error) {
	var keyEnc []byte
	var regJSON []byte
	err := m.pool.QueryRow(ctx, `select key_enc, registration from acme_accounts where directory=$1`, m.directory).Scan(&keyEnc, &regJSON)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	acc := &account{email: m.email}
	if err == nil {
		der, err := m.key.Open(keyEnc)
		if err != nil {
			return nil, err
		}
		acc.key, err = x509.ParseECPrivateKey(der)
		if err != nil {
			return nil, err
		}
		if len(regJSON) > 0 {
			_ = json.Unmarshal(regJSON, &acc.reg)
		}
		return acc, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	acc.key = k
	return acc, nil
}

func (m *Manager) saveAccount(ctx context.Context, acc *account) error {
	der, err := x509.MarshalECPrivateKey(acc.key)
	if err != nil {
		return err
	}
	enc, err := m.key.Seal(der)
	if err != nil {
		return err
	}
	regJSON, _ := json.Marshal(acc.reg)
	_, err = m.pool.Exec(ctx, `insert into acme_accounts(directory, email, key_enc, registration) values($1,$2,$3,$4)
		on conflict (directory) do update set email=excluded.email, key_enc=excluded.key_enc, registration=excluded.registration`, m.directory, m.email, enc, regJSON)
	return err
}

// ---- provider HTTP-01 ----

type provider struct{ m *Manager }

func (p provider) Present(domain, token, keyAuth string) error {
	_, err := p.m.pool.Exec(context.Background(), `insert into acme_challenges(token, key_auth, fqdn) values($1,$2,$3) on conflict (token) do update set key_auth=excluded.key_auth`, token, keyAuth, domain)
	return err
}

func (p provider) CleanUp(domain, token, keyAuth string) error {
	_, err := p.m.pool.Exec(context.Background(), `delete from acme_challenges where token=$1`, token)
	return err
}

// Handler sert les challenges (monté sur /.well-known/acme-challenge/).
func (m *Manager) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		var ka string
		if err := m.pool.QueryRow(r.Context(), `select key_auth from acme_challenges where token=$1`, token).Scan(&ka); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(ka))
	}
}

// ---- émission ----

func (m *Manager) client(ctx context.Context) (*lego.Client, error) {
	acc, err := m.loadAccount(ctx)
	if err != nil {
		return nil, err
	}
	cfg := lego.NewConfig(acc)
	cfg.CADirURL = m.directory
	cfg.Certificate.KeyType = certcrypto.EC256
	cfg.HTTPClient.Timeout = 60 * time.Second
	cl, err := lego.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := cl.Challenge.SetHTTP01Provider(provider{m}); err != nil {
		return nil, err
	}
	if acc.reg == nil {
		reg, err := cl.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, err
		}
		acc.reg = reg
		if err := m.saveAccount(ctx, acc); err != nil {
			return nil, err
		}
		m.log.Info("compte ACME enregistré", "directory", m.directory)
	}
	return cl, nil
}

// Issue obtient (ou renouvelle) le certificat d'un domaine et l'enregistre.
func (m *Manager) Issue(ctx context.Context, domainID, fqdn string) error {
	_, _ = m.pool.Exec(ctx, `insert into certificates(domain_id, fqdn, status, last_attempt_at) values($1,$2,'pending',now())
		on conflict (domain_id) do update set last_attempt_at=now()`, domainID, fqdn)
	cl, err := m.client(ctx)
	if err != nil {
		return m.fail(ctx, domainID, err)
	}
	res, err := cl.Certificate.Obtain(certificate.ObtainRequest{Domains: []string{fqdn}, Bundle: true})
	if err != nil {
		return m.fail(ctx, domainID, err)
	}
	full := append(append([]byte{}, res.Certificate...), res.PrivateKey...) // HAProxy : chaîne + clé dans un seul PEM
	block, _ := pem.Decode(res.Certificate)
	var notBefore, notAfter time.Time
	var issuer string
	if block != nil {
		if crt, err := x509.ParseCertificate(block.Bytes); err == nil {
			notBefore, notAfter, issuer = crt.NotBefore, crt.NotAfter, crt.Issuer.CommonName
		}
	}
	enc, err := m.key.Seal(full)
	if err != nil {
		return m.fail(ctx, domainID, err)
	}
	if _, err := m.pool.Exec(ctx, `update certificates set pem_enc=$2, status='issued', last_error=null, issuer=$3, not_before=$4, not_after=$5, updated_at=now() where domain_id=$1`,
		domainID, enc, issuer, notBefore, notAfter); err != nil {
		return err
	}
	m.log.Info("certificat émis", "fqdn", fqdn, "issuer", issuer, "not_after", notAfter.Format(time.RFC3339))
	if m.onIssued != nil {
		m.onIssued()
	}
	return nil
}

func (m *Manager) fail(ctx context.Context, domainID string, err error) error {
	msg := err.Error()
	if len(msg) > 900 {
		msg = msg[:900]
	}
	_, _ = m.pool.Exec(ctx, `update certificates set status='error', last_error=$2, updated_at=now() where domain_id=$1`, domainID, msg)
	return err
}

// Run : toutes les 10 min, émet les certificats manquants (tls=auto) et renouvelle ceux à moins de 30 jours.
// Les échecs sont retentés au plus toutes les 15 min par domaine.
func (m *Manager) Run(ctx context.Context) {
	if !m.Enabled() {
		m.log.Warn("ACME désactivé : FLYC_ACME_EMAIL non défini, les domaines garderont le certificat de démarrage")
		return
	}
	for {
		m.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

// Trigger lance une passe immédiate (après création d'un domaine).
func (m *Manager) Trigger(ctx context.Context) { go m.pass(ctx) }

func (m *Manager) pass(ctx context.Context) {
	rows, err := m.pool.Query(ctx, `
		select d.id, d.fqdn from domains d
		left join certificates c on c.domain_id = d.id
		where d.tls = 'auto' and d.mode = 'dns'
		  and (c.id is null or c.status <> 'issued' or c.not_after < now() + interval '30 days')
		  and (c.last_attempt_at is null or c.last_attempt_at < now() - interval '15 minutes')`)
	if err != nil {
		m.log.Warn("acme: lecture des domaines", "err", err)
		return
	}
	type d struct{ id, fqdn string }
	var todo []d
	for rows.Next() {
		var x d
		if rows.Scan(&x.id, &x.fqdn) == nil {
			todo = append(todo, x)
		}
	}
	rows.Close()
	for _, x := range todo {
		if err := m.Issue(ctx, x.id, x.fqdn); err != nil {
			m.log.Warn("acme: émission échouée", "fqdn", x.fqdn, "err", err)
		}
	}
}
