// Package api : API REST v1 de flyc-control (clé API plateforme ou tenant).
//
//	Plateforme : POST/GET /v1/tenants, POST /v1/tenants/{slug}/api-keys, GET/DELETE /v1/edges,
//	             GET/DELETE /v1/queue-hosts, POST /v1/sync
//	Tenant     : /v1/domains, /v1/backends, /v1/rooms (+ /state, /stats, /flush), /v1/domains/{fqdn}/certificate
//
// Une clé plateforme agit sur un tenant en passant X-Flyc-Tenant: <slug>.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flyc-io/flyc/control/internal/acme"
	"github.com/flyc-io/flyc/control/internal/auth"
	"github.com/flyc-io/flyc/control/internal/bootstrap"
	"github.com/flyc-io/flyc/control/internal/crypt"
	"github.com/flyc-io/flyc/control/internal/jobs"
	"github.com/flyc-io/flyc/control/internal/queueclient"
	"github.com/flyc-io/flyc/control/internal/reconcile"
	"github.com/flyc-io/flyc/internal/httpx"
)

type API struct {
	pool     *pgxpool.Pool
	rec      *reconcile.Reconciler
	acme     *acme.Manager
	queue    *queueclient.Client
	log      *slog.Logger
	jobs     *jobs.Manager
	sessions *auth.Sessions
	oidc     *auth.OIDC
	key      crypt.Key
	issuer   string // nom affiché dans les applications TOTP
	mu       sync.RWMutex
	signing  *bootstrap.Result // clé active servie par /internal/signing-key et le JWKS
}

// SetSigning fixe la clé active ; Signing la lit.
func (a *API) SetSigning(res *bootstrap.Result)   { a.mu.Lock(); a.signing = res; a.mu.Unlock() }
func (a *API) Signing() *bootstrap.Result         { a.mu.RLock(); defer a.mu.RUnlock(); return a.signing }
func (a *API) onKeyRotated(res *bootstrap.Result) { a.SetSigning(res) }

func New(pool *pgxpool.Pool, rec *reconcile.Reconciler, ac *acme.Manager, q *queueclient.Client, sessions *auth.Sessions, oidc *auth.OIDC, key crypt.Key, issuer string, log *slog.Logger) *API {
	return &API{pool: pool, rec: rec, acme: ac, queue: q, log: log, sessions: sessions, oidc: oidc, key: key, issuer: issuer}
}

// SetJobs branche l'exécuteur de tâches. Il est optionnel : sans runner (installation par
// fichier, ou hôte sans le profil data), l'API répond 503 sur ces routes et le reste fonctionne.
func (a *API) SetJobs(m *jobs.Manager) { a.jobs = m }

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var fqdnRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// ---- auth ----

type principal struct {
	keyID    string
	tenantID string // vide = plateforme
	tenant   string
	platform bool
	role     string     // owner | admin | operator | viewer (session utilisateur sur un tenant)
	user     *auth.User // nil pour une clé API
	session  bool
}

var roleRank = map[string]int{"viewer": 1, "operator": 2, "admin": 3, "owner": 4}

func (p *principal) allows(minRole string) bool {
	if !p.session || p.platform {
		return true // clés API et administrateurs plateforme : accès complet
	}
	return roleRank[p.role] >= roleRank[minRole]
}

func HashKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])
}

// NewKey génère une clé "flyc_<40 hex>".
func NewKey() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return "flyc_" + hex.EncodeToString(b)
}

func (a *API) authenticate(r *http.Request) (*principal, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return a.authenticateSession(r)
	}
	key := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	var p principal
	var tenantID *string
	err := a.pool.QueryRow(r.Context(), `select k.id, k.tenant_id, coalesce(t.slug,'') from api_keys k left join tenants t on t.id=k.tenant_id where k.key_hash=$1`, HashKey(key)).Scan(&p.keyID, &tenantID, &p.tenant)
	if err != nil {
		return nil, errors.New("clé API invalide")
	}
	_, _ = a.pool.Exec(r.Context(), `update api_keys set last_used_at=now() where id=$1`, p.keyID)
	if tenantID == nil {
		p.platform = true
		if slug := r.Header.Get("X-Flyc-Tenant"); slug != "" {
			if err := a.pool.QueryRow(r.Context(), `select id, slug from tenants where slug=$1`, slug).Scan(&p.tenantID, &p.tenant); err != nil {
				return nil, errors.New("tenant inconnu")
			}
		}
	} else {
		p.tenantID = *tenantID
	}
	return &p, nil
}

type handler func(w http.ResponseWriter, r *http.Request, p *principal)

func (a *API) platform(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			httpx.Error(w, 401, "unauthorized", err.Error())
			return
		}
		if !p.platform {
			httpx.Error(w, 403, "forbidden", "clé plateforme requise")
			return
		}
		h(w, r, p)
	}
}

// tenant : route de tenant, avec le rôle minimal requis pour une session utilisateur.
func (a *API) tenant(minRole string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			httpx.Error(w, 401, "unauthorized", err.Error())
			return
		}
		if p.tenantID == "" {
			httpx.Error(w, 400, "no_tenant", "précisez le tenant avec X-Flyc-Tenant")
			return
		}
		if !p.allows(minRole) {
			httpx.Error(w, 403, "forbidden", "rôle insuffisant : "+minRole+" requis")
			return
		}
		h(w, r, p)
	}
}

// session : route réservée aux utilisateurs connectés (compte).
func (a *API) session(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticateSession(r)
		if err != nil {
			httpx.Error(w, 401, "unauthorized", err.Error())
			return
		}
		h(w, r, p)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		httpx.Error(w, 400, "bad_json", err.Error())
		return false
	}
	return true
}

func dbErr(w http.ResponseWriter, err error) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			httpx.Error(w, 409, "conflict", "existe déjà : "+pe.ConstraintName)
			return
		case "23514", "22P02":
			httpx.Error(w, 400, "invalid", pe.Message)
			return
		case "23503":
			httpx.Error(w, 400, "invalid_reference", pe.Message)
			return
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, 404, "not_found", "introuvable")
		return
	}
	httpx.Error(w, 500, "db", err.Error())
}

func (a *API) audit(ctx context.Context, p *principal, action string, diff any) {
	b, _ := json.Marshal(diff)
	var tid *string
	if p.tenantID != "" {
		tid = &p.tenantID
	}
	_, _ = a.pool.Exec(ctx, `insert into audit_log(tenant_id, action, diff) values($1,$2,$3)`, tid, action, b)
}

// Register monte les routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			httpx.Error(w, 401, "unauthorized", err.Error())
			return
		}
		httpx.JSON(w, 200, map[string]any{"platform": p.platform, "tenant": p.tenant, "role": p.role, "session": p.session})
	})
	mux.HandleFunc("POST /v1/tenants", a.platform(a.createTenant))
	mux.HandleFunc("GET /v1/tenants", a.platform(a.listTenants))
	mux.HandleFunc("POST /v1/tenants/{slug}/api-keys", a.platform(a.createTenantKey))
	mux.HandleFunc("GET /v1/edges", a.platform(a.listEdges))
	mux.HandleFunc("DELETE /v1/edges/{name}", a.platform(a.deleteEdge))
	mux.HandleFunc("GET /v1/queue-hosts", a.platform(a.listQueueHosts))
	mux.HandleFunc("DELETE /v1/queue-hosts/{name}", a.platform(a.deleteQueueHost))
	a.registerJobs(mux)
	a.registerInventory(mux)
	a.registerEnroll(mux)
	mux.HandleFunc("POST /v1/sync", a.platform(func(w http.ResponseWriter, r *http.Request, p *principal) {
		a.rec.Once(r.Context(), true)
		a.acme.Trigger(context.Background())
		httpx.JSON(w, 200, map[string]string{"status": "synced"})
	}))

	mux.HandleFunc("GET /v1/domains", a.tenant("viewer", a.listDomains))
	mux.HandleFunc("POST /v1/domains", a.tenant("admin", a.createDomain))
	mux.HandleFunc("DELETE /v1/domains/{fqdn}", a.tenant("admin", a.deleteDomain))
	mux.HandleFunc("GET /v1/domains/{fqdn}/certificate", a.tenant("viewer", a.domainCert))
	mux.HandleFunc("POST /v1/domains/{fqdn}/certificate/renew", a.tenant("admin", a.renewCert))

	mux.HandleFunc("GET /v1/backends", a.tenant("viewer", a.listBackends))
	mux.HandleFunc("POST /v1/backends", a.tenant("admin", a.upsertBackend))
	mux.HandleFunc("PUT /v1/backends/{name}", a.tenant("admin", a.upsertBackend))
	mux.HandleFunc("DELETE /v1/backends/{name}", a.tenant("admin", a.deleteBackend))

	mux.HandleFunc("GET /v1/rooms", a.tenant("viewer", a.listRooms))
	mux.HandleFunc("POST /v1/rooms", a.tenant("admin", a.upsertRoom))
	mux.HandleFunc("PUT /v1/rooms/{slug}", a.tenant("admin", a.upsertRoom))
	mux.HandleFunc("DELETE /v1/rooms/{slug}", a.tenant("admin", a.deleteRoom))
	mux.HandleFunc("POST /v1/rooms/{slug}/state", a.tenant("operator", a.roomState))
	mux.HandleFunc("GET /v1/rooms/{slug}/stats", a.tenant("viewer", a.roomStats))
	mux.HandleFunc("POST /v1/rooms/{slug}/flush", a.tenant("operator", a.roomFlush))
	mux.HandleFunc("POST /v1/pass/verify", a.tenant("viewer", a.verifyPass))
	mux.HandleFunc("GET /v1/api-keys", a.tenant("admin", a.listTenantKeys))
	mux.HandleFunc("POST /v1/api-keys", a.tenant("admin", a.createOwnTenantKey))
	mux.HandleFunc("DELETE /v1/api-keys/{id}", a.tenant("admin", a.deleteTenantKey))

	a.registerAuth(mux)
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, a.jwks(r.Context()))
	})
	mux.HandleFunc("GET /.well-known/flyc-pass.pem", func(w http.ResponseWriter, _ *http.Request) {
		s := a.Signing()
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("X-Flyc-Kid", s.KID)
		_, _ = w.Write([]byte(s.PublicPEM))
	})
	mux.HandleFunc("GET /internal/signing-key", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+os.Getenv("FLYC_INTERNAL_TOKEN") {
			httpx.Error(w, http.StatusUnauthorized, "unauthorized", "jeton interne invalide")
			return
		}
		s := a.Signing()
		httpx.JSON(w, 200, map[string]string{"kid": s.KID, "private_pem": s.PrivatePEM, "public_pem": s.PublicPEM})
	})
}

// jwks : clé active + clés retirées depuis moins de 24 h.
func (a *API) jwks(ctx context.Context) map[string]any {
	rows, err := a.pool.Query(ctx, `select kid, public_pem from signing_keys where active or retired_at > now() - interval '24 hours' order by active desc, created_at desc`)
	keys := []any{}
	if err == nil {
		for rows.Next() {
			var kid, pem string
			if rows.Scan(&kid, &pem) == nil {
				if j, err := bootstrap.JWK(kid, pem); err == nil {
					keys = append(keys, j)
				}
			}
		}
		rows.Close()
	}
	return map[string]any{"keys": keys}
}

// ---- plateforme ----

func (a *API) createTenant(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !slugRe.MatchString(in.Slug) {
		httpx.Error(w, 400, "invalid", "slug invalide")
		return
	}
	if in.Name == "" {
		in.Name = in.Slug
	}
	var id string
	if err := a.pool.QueryRow(r.Context(), `insert into tenants(slug, name) values($1,$2) returning id`, in.Slug, in.Name).Scan(&id); err != nil {
		dbErr(w, err)
		return
	}
	key := NewKey()
	if _, err := a.pool.Exec(r.Context(), `insert into api_keys(tenant_id, name, key_hash) values($1,'initial',$2)`, id, HashKey(key)); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "tenant.create", in)
	httpx.JSON(w, 201, map[string]any{"id": id, "slug": in.Slug, "name": in.Name, "api_key": key})
}

func (a *API) listTenants(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select t.slug, t.name, t.created_at, (select count(*) from rooms where tenant_id=t.id), (select count(*) from domains where tenant_id=t.id) from tenants t order by slug`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var slug, name string
		var created time.Time
		var nr, nd int
		_ = rows.Scan(&slug, &name, &created, &nr, &nd)
		out = append(out, map[string]any{"slug": slug, "name": name, "created_at": created, "rooms": nr, "domains": nd})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) createTenantKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Name == "" {
		in.Name = "key-" + time.Now().Format("20060102-150405")
	}
	var id string
	if err := a.pool.QueryRow(r.Context(), `select id from tenants where slug=$1`, r.PathValue("slug")).Scan(&id); err != nil {
		dbErr(w, err)
		return
	}
	key := NewKey()
	if _, err := a.pool.Exec(r.Context(), `insert into api_keys(tenant_id, name, key_hash) values($1,$2,$3)`, id, in.Name, HashKey(key)); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "apikey.create", map[string]string{"tenant": r.PathValue("slug"), "name": in.Name})
	httpx.JSON(w, 201, map[string]string{"name": in.Name, "api_key": key})
}

func (a *API) listEdges(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select name, dataplane_url, coalesce(last_push_status,''), coalesce(last_error,''), last_push_at, coalesce(config_hash,''), coalesce(desired_hash,'') from edges order by name`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, u, st, e, ch, dh string
		var at *time.Time
		_ = rows.Scan(&name, &u, &st, &e, &at, &ch, &dh)
		out = append(out, map[string]any{"name": name, "dataplane_url": u, "last_push_status": st, "last_error": e, "last_push_at": at, "in_sync": ch != "" && ch == dh})
	}
	httpx.JSON(w, 200, out)
}

// deleteEdge retire de la base un LB sorti de l'inventaire, pour qu'il cesse d'apparaître dans
// l'interface. Tant qu'il figure dans la configuration, la ligne serait recréée au prochain
// démarrage : on refuse plutôt que de laisser croire au retrait.
func (a *API) deleteEdge(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	for _, n := range a.rec.EdgeNames() {
		if n == name {
			httpx.Error(w, 409, "still_configured",
				"ce LB est encore déployé : le retirer de l'inventaire et rejouer le playbook d'abord")
			return
		}
	}
	tag, err := a.pool.Exec(r.Context(), `delete from edges where name=$1`, name)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "aucun LB de ce nom")
		return
	}
	a.audit(r.Context(), p, "edge.delete", map[string]string{"name": name})
	httpx.JSON(w, 200, map[string]string{"deleted": name})
}

// deleteQueueHost : même principe pour un hôte de file.
func (a *API) deleteQueueHost(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	var addr string
	var port int
	err := a.pool.QueryRow(r.Context(), `select address, queue_port from queue_hosts where name=$1`, name).Scan(&addr, &port)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, 404, "not_found", "aucun hôte de file de ce nom")
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	url := "http://" + addr + ":" + itoa(port)
	for _, h := range a.queue.Hosts() {
		if h == url {
			httpx.Error(w, 409, "still_configured",
				"cet hôte de file est encore déployé : le retirer de l'inventaire et rejouer le playbook d'abord")
			return
		}
	}
	if _, err := a.pool.Exec(r.Context(), `delete from queue_hosts where name=$1`, name); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "queuehost.delete", map[string]string{"name": name})
	httpx.JSON(w, 200, map[string]string{"deleted": name})
}

func (a *API) listQueueHosts(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select name, address, queue_port, last_seen_at, coalesce(last_error,'') from queue_hosts order by name`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, addr, e string
		var port int
		var seen *time.Time
		_ = rows.Scan(&name, &addr, &port, &seen, &e)
		h := "http://" + addr + ":" + itoa(port)
		health := "ok"
		if err := a.queue.Healthy(r.Context(), h); err != nil {
			health = err.Error()
		}
		out = append(out, map[string]any{"name": name, "address": addr, "port": port, "last_seen_at": seen, "last_error": e, "health": health})
	}
	httpx.JSON(w, 200, out)
}

func itoa(i int) string { return strconv.Itoa(i) }
