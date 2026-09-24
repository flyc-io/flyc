package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/flyc-io/flyc/control/internal/compile"
	"github.com/flyc-io/flyc/internal/httpx"
	"github.com/flyc-io/flyc/internal/pass"
)

// ---- domaines ----

func (a *API) listDomains(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := a.pool.Query(r.Context(), `select d.fqdn, d.mode, d.tls, d.created_at, coalesce(c.status,'none'), c.not_after, coalesce(c.last_error,''), (select slug from rooms where domain_id=d.id)
		from domains d left join certificates c on c.domain_id=d.id where d.tenant_id=$1 order by d.fqdn`, p.tenantID)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var fqdn, mode, tls, cstat, cerr string
		var created time.Time
		var notAfter *time.Time
		var room *string
		_ = rows.Scan(&fqdn, &mode, &tls, &created, &cstat, &notAfter, &cerr, &room)
		out = append(out, map[string]any{"fqdn": fqdn, "mode": mode, "tls": tls, "created_at": created, "certificate": map[string]any{"status": cstat, "not_after": notAfter, "last_error": cerr}, "room": room})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) createDomain(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		FQDN string `json:"fqdn"`
		Mode string `json:"mode"`
		TLS  string `json:"tls"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.FQDN = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.FQDN), "."))
	if !fqdnRe.MatchString(in.FQDN) {
		httpx.Error(w, 400, "invalid", "fqdn invalide")
		return
	}
	if in.Mode == "" {
		in.Mode = "dns"
	}
	if in.TLS == "" {
		in.TLS = "auto"
	}
	if _, err := a.pool.Exec(r.Context(), `insert into domains(tenant_id, fqdn, mode, tls) values($1,$2,$3,$4)`, p.tenantID, in.FQDN, in.Mode, in.TLS); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "domain.create", in)
	a.acme.Trigger(context.Background())
	httpx.JSON(w, 201, in)
}

func (a *API) deleteDomain(w http.ResponseWriter, r *http.Request, p *principal) {
	tag, err := a.pool.Exec(r.Context(), `delete from domains where tenant_id=$1 and fqdn=$2`, p.tenantID, strings.ToLower(r.PathValue("fqdn")))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "domaine inconnu")
		return
	}
	a.audit(r.Context(), p, "domain.delete", r.PathValue("fqdn"))
	a.rec.Kick()
	w.WriteHeader(204)
}

func (a *API) domainCert(w http.ResponseWriter, r *http.Request, p *principal) {
	var status, issuer, lastErr string
	var notBefore, notAfter, attempt *time.Time
	err := a.pool.QueryRow(r.Context(), `select coalesce(c.status,'none'), coalesce(c.issuer,''), c.not_before, c.not_after, coalesce(c.last_error,''), c.last_attempt_at
		from domains d left join certificates c on c.domain_id=d.id where d.tenant_id=$1 and d.fqdn=$2`, p.tenantID, strings.ToLower(r.PathValue("fqdn"))).Scan(&status, &issuer, &notBefore, &notAfter, &lastErr, &attempt)
	if err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]any{"status": status, "issuer": issuer, "not_before": notBefore, "not_after": notAfter, "last_error": lastErr, "last_attempt_at": attempt})
}

func (a *API) renewCert(w http.ResponseWriter, r *http.Request, p *principal) {
	var id, fqdn string
	if err := a.pool.QueryRow(r.Context(), `select id, fqdn from domains where tenant_id=$1 and fqdn=$2`, p.tenantID, strings.ToLower(r.PathValue("fqdn"))).Scan(&id, &fqdn); err != nil {
		dbErr(w, err)
		return
	}
	if !a.acme.Enabled() {
		httpx.Error(w, 409, "acme_disabled", "ACME n'est pas configuré (FLYC_ACME_EMAIL)")
		return
	}
	go func() {
		if err := a.acme.Issue(context.Background(), id, fqdn); err != nil {
			a.log.Warn("renouvellement demandé : échec", "fqdn", fqdn, "err", err)
		}
	}()
	httpx.JSON(w, 202, map[string]string{"status": "requested"})
}

// ---- backends ----

type backendIn struct {
	Name      string `json:"name"`
	Balance   string `json:"balance"`
	CheckPath string `json:"check_path"`
	Servers   []struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
		Weight  *int   `json:"weight"`
		Maxconn *int   `json:"maxconn"`
	} `json:"servers"`
}

func (a *API) listBackends(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := a.pool.Query(r.Context(), `select b.id, b.name, b.balance, b.check_path from backends b where tenant_id=$1 order by name`, p.tenantID)
	if err != nil {
		dbErr(w, err)
		return
	}
	type bk struct {
		ID, Name, Balance, Check string
	}
	var bks []bk
	for rows.Next() {
		var b bk
		_ = rows.Scan(&b.ID, &b.Name, &b.Balance, &b.Check)
		bks = append(bks, b)
	}
	rows.Close()
	out := []map[string]any{}
	for _, b := range bks {
		srows, _ := a.pool.Query(r.Context(), `select name, address, port, weight, maxconn from backend_servers where backend_id=$1 order by name`, b.ID)
		servers := []map[string]any{}
		for srows.Next() {
			var n, addr string
			var port, wgt, mc int
			_ = srows.Scan(&n, &addr, &port, &wgt, &mc)
			servers = append(servers, map[string]any{"name": n, "address": addr, "port": port, "weight": wgt, "maxconn": mc})
		}
		srows.Close()
		out = append(out, map[string]any{"name": b.Name, "balance": b.Balance, "check_path": b.Check, "haproxy_name": compileBackendName(p.tenant, b.Name), "servers": servers})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) upsertBackend(w http.ResponseWriter, r *http.Request, p *principal) {
	var in backendIn
	if !decode(w, r, &in) {
		return
	}
	if n := r.PathValue("name"); n != "" {
		in.Name = n
	}
	if !slugRe.MatchString(in.Name) {
		httpx.Error(w, 400, "invalid", "nom de backend invalide")
		return
	}
	if in.Balance == "" {
		in.Balance = "roundrobin"
	}
	if in.CheckPath == "" {
		in.CheckPath = "/"
	}
	if len(in.Servers) == 0 {
		httpx.Error(w, 400, "invalid", "au moins un serveur")
		return
	}
	tx, err := a.pool.Begin(r.Context())
	if err != nil {
		dbErr(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var id string
	if err := tx.QueryRow(r.Context(), `insert into backends(tenant_id, name, balance, check_path) values($1,$2,$3,$4)
		on conflict (tenant_id, name) do update set balance=excluded.balance, check_path=excluded.check_path returning id`, p.tenantID, in.Name, in.Balance, in.CheckPath).Scan(&id); err != nil {
		dbErr(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `delete from backend_servers where backend_id=$1`, id); err != nil {
		dbErr(w, err)
		return
	}
	for i, s := range in.Servers {
		if s.Name == "" {
			s.Name = "srv" + itoa(i+1)
		}
		wgt, mc := 100, 0
		if s.Weight != nil {
			wgt = *s.Weight
		}
		if s.Maxconn != nil {
			mc = *s.Maxconn
		}
		if _, err := tx.Exec(r.Context(), `insert into backend_servers(backend_id, name, address, port, weight, maxconn) values($1,$2,$3,$4,$5,$6)`, id, s.Name, s.Address, s.Port, wgt, mc); err != nil {
			dbErr(w, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "backend.upsert", in)
	a.rec.Kick()
	httpx.JSON(w, 200, map[string]any{"name": in.Name, "haproxy_name": compileBackendName(p.tenant, in.Name), "servers": len(in.Servers)})
}

func (a *API) deleteBackend(w http.ResponseWriter, r *http.Request, p *principal) {
	tag, err := a.pool.Exec(r.Context(), `delete from backends where tenant_id=$1 and name=$2`, p.tenantID, r.PathValue("name"))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "backend inconnu")
		return
	}
	a.audit(r.Context(), p, "backend.delete", r.PathValue("name"))
	a.rec.Kick()
	w.WriteHeader(204)
}

// ---- files ----

type roomIn struct {
	Slug      string            `json:"slug"`
	Title     string            `json:"title"`
	Domain    string            `json:"domain"`
	Backend   string            `json:"backend"`
	State     string            `json:"state"`
	Rate      float64           `json:"rate"`
	MaxActive int64             `json:"max_active"`
	WaveS     int               `json:"wave_s"`
	PassTTLS  int               `json:"pass_ttl_s"`
	OpensAt   *string           `json:"opens_at"`
	Theme     map[string]string `json:"theme"`
	AutoOn    int64             `json:"auto_on"`
	AutoOff   int64             `json:"auto_off"`
}

func (a *API) listRooms(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := a.pool.Query(r.Context(), `select r.slug, r.title, r.state, r.rate, r.max_active, r.wave_s, r.pass_ttl_s, r.opens_at, d.fqdn, b.name, r.updated_at, r.theme, r.effective_state, r.auto_on, r.auto_off
		from rooms r left join domains d on d.id=r.domain_id left join backends b on b.id=r.backend_id where r.tenant_id=$1 order by r.slug`, p.tenantID)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var slug, title, state string
		var rate float64
		var maxActive int64
		var waveS, ttl int
		var opens, updated *time.Time
		var fqdn, bname *string
		var theme map[string]string
		var eff string
		var autoOn, autoOff int64
		_ = rows.Scan(&slug, &title, &state, &rate, &maxActive, &waveS, &ttl, &opens, &fqdn, &bname, &updated, &theme, &eff, &autoOn, &autoOff)
		if theme == nil {
			theme = map[string]string{}
		}
		out = append(out, map[string]any{"slug": slug, "id": p.tenant + "." + slug, "title": title, "state": state, "effective_state": eff, "auto_on": autoOn, "auto_off": autoOff, "rate": rate, "max_active": maxActive, "wave_s": waveS, "pass_ttl_s": ttl, "opens_at": opens, "domain": fqdn, "backend": bname, "updated_at": updated, "theme": theme})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) upsertRoom(w http.ResponseWriter, r *http.Request, p *principal) {
	var in roomIn
	if !decode(w, r, &in) {
		return
	}
	if s := r.PathValue("slug"); s != "" {
		in.Slug = s
	}
	if !slugRe.MatchString(in.Slug) {
		httpx.Error(w, 400, "invalid", "slug invalide")
		return
	}
	if in.State == "" {
		in.State = "open"
	}
	if in.Rate <= 0 {
		in.Rate = 10
	}
	if in.WaveS <= 0 {
		in.WaveS = 30
	}
	if in.PassTTLS <= 0 {
		in.PassTTLS = 1200
	}
	var domainID, backendID *string
	if in.Domain != "" {
		var id string
		if err := a.pool.QueryRow(r.Context(), `select id from domains where tenant_id=$1 and fqdn=$2`, p.tenantID, strings.ToLower(in.Domain)).Scan(&id); err != nil {
			httpx.Error(w, 400, "invalid_domain", "domaine inconnu pour ce tenant : "+in.Domain)
			return
		}
		domainID = &id
	}
	if in.Backend != "" {
		var id string
		if err := a.pool.QueryRow(r.Context(), `select id from backends where tenant_id=$1 and name=$2`, p.tenantID, in.Backend).Scan(&id); err != nil {
			httpx.Error(w, 400, "invalid_backend", "backend inconnu pour ce tenant : "+in.Backend)
			return
		}
		backendID = &id
	}
	var opens *time.Time
	if in.OpensAt != nil && *in.OpensAt != "" {
		t, err := time.Parse(time.RFC3339, *in.OpensAt)
		if err != nil {
			httpx.Error(w, 400, "invalid", "opens_at doit être au format RFC3339")
			return
		}
		opens = &t
	}
	if in.Theme == nil {
		in.Theme = map[string]string{}
	}
	for k := range in.Theme {
		switch k {
		case "accent", "logo_url", "message", "lang":
		default:
			delete(in.Theme, k)
		}
	}
	if in.State == "auto" && (in.AutoOn <= 0 || in.AutoOff < 0 || in.AutoOff >= in.AutoOn) {
		httpx.Error(w, 400, "invalid", "mode auto : auto_on > auto_off ≥ 0 requis (connexions backend)")
		return
	}
	eff := in.State
	if in.State == "auto" {
		eff = "open" // état effectif initial, ajusté par la boucle auto
	}
	_, err := a.pool.Exec(r.Context(), `insert into rooms(tenant_id, slug, title, domain_id, backend_id, state, rate, max_active, wave_s, pass_ttl_s, opens_at, theme, auto_on, auto_off, effective_state)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		on conflict (tenant_id, slug) do update set title=excluded.title, domain_id=excluded.domain_id, backend_id=excluded.backend_id, state=excluded.state,
		  rate=excluded.rate, max_active=excluded.max_active, wave_s=excluded.wave_s, pass_ttl_s=excluded.pass_ttl_s, opens_at=excluded.opens_at, theme=excluded.theme,
		  auto_on=excluded.auto_on, auto_off=excluded.auto_off,
		  effective_state=case when excluded.state='auto' then rooms.effective_state else excluded.state end, updated_at=now()`,
		p.tenantID, in.Slug, in.Title, domainID, backendID, in.State, in.Rate, in.MaxActive, in.WaveS, in.PassTTLS, opens, in.Theme, in.AutoOn, in.AutoOff, eff)
	if err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "room.upsert", in)
	a.rec.Kick()
	httpx.JSON(w, 200, map[string]any{"id": p.tenant + "." + in.Slug, "state": in.State})
}

func (a *API) deleteRoom(w http.ResponseWriter, r *http.Request, p *principal) {
	tag, err := a.pool.Exec(r.Context(), `delete from rooms where tenant_id=$1 and slug=$2`, p.tenantID, r.PathValue("slug"))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "file inconnue")
		return
	}
	a.audit(r.Context(), p, "room.delete", r.PathValue("slug"))
	a.rec.Kick()
	w.WriteHeader(204)
}

// roomState : bascule open / queue / closed. Appliquée à chaud (maps HAProxy + flyc-queue), sans reload.
func (a *API) roomState(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		State string `json:"state"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.State != "open" && in.State != "queue" && in.State != "closed" && in.State != "auto" {
		httpx.Error(w, 400, "invalid", "state doit être open, queue, closed ou auto")
		return
	}
	var tag pgconn.CommandTag
	var err error
	if in.State == "auto" {
		tag, err = a.pool.Exec(r.Context(), `update rooms set state='auto', updated_at=now() where tenant_id=$1 and slug=$2 and auto_on > 0`, p.tenantID, r.PathValue("slug"))
		if err == nil && tag.RowsAffected() == 0 {
			httpx.Error(w, 400, "invalid", "définissez d'abord les seuils auto_on / auto_off de la file")
			return
		}
	} else {
		tag, err = a.pool.Exec(r.Context(), `update rooms set state=$3, effective_state=$3, updated_at=now() where tenant_id=$1 and slug=$2`, p.tenantID, r.PathValue("slug"), in.State)
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "file inconnue")
		return
	}
	a.audit(r.Context(), p, "room.state", map[string]string{"slug": r.PathValue("slug"), "state": in.State})
	a.rec.Once(r.Context(), true) // synchrone : la réponse garantit que les LB ont basculé
	httpx.JSON(w, 200, map[string]string{"id": p.tenant + "." + r.PathValue("slug"), "state": in.State})
}

func (a *API) roomStats(w http.ResponseWriter, r *http.Request, p *principal) {
	code, body, err := a.queue.Call(r.Context(), http.MethodGet, "/internal/rooms/"+p.tenant+"."+r.PathValue("slug"))
	if err != nil {
		httpx.Error(w, 503, "queue_unreachable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (a *API) roomFlush(w http.ResponseWriter, r *http.Request, p *principal) {
	code, body, err := a.queue.Call(r.Context(), http.MethodPost, "/internal/rooms/"+p.tenant+"."+r.PathValue("slug")+"/flush")
	if err != nil {
		httpx.Error(w, 503, "queue_unreachable", err.Error())
		return
	}
	a.audit(r.Context(), p, "room.flush", r.PathValue("slug"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func compileBackendName(tenant, name string) string { return compile.BackendName(tenant, name) }

// verifyPass : vérification serveur d'un pass (mode iframe / JS). Signature avec les clés actives,
// expiration, et le domaine et la file doivent appartenir au tenant appelant.
func (a *API) verifyPass(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Pass string `json:"pass"`
		Host string `json:"host"`
	}
	if !decode(w, r, &in) {
		return
	}
	rows, err := a.pool.Query(r.Context(), `select public_pem from signing_keys where active or retired_at > now() - interval '1 day'`)
	if err != nil {
		dbErr(w, err)
		return
	}
	var pems []string
	for rows.Next() {
		var pem string
		if rows.Scan(&pem) == nil {
			pems = append(pems, pem)
		}
	}
	rows.Close()
	var claims *pass.Payload
	var lastErr error
	for _, pem := range pems {
		if c, err := pass.Verify(pem, in.Pass, strings.ToLower(in.Host)); err == nil {
			claims = c
			break
		} else {
			lastErr = err
		}
	}
	if claims == nil {
		msg := "pass invalide"
		if lastErr != nil {
			msg = lastErr.Error()
		}
		httpx.JSON(w, 200, map[string]any{"valid": false, "reason": msg})
		return
	}
	var n int
	_ = a.pool.QueryRow(r.Context(), `select count(*) from domains where tenant_id=$1 and fqdn=$2`, p.tenantID, claims.Dom).Scan(&n)
	if n == 0 || !strings.HasPrefix(claims.Rm, p.tenant+".") {
		httpx.JSON(w, 200, map[string]any{"valid": false, "reason": "pass émis pour un autre tenant"})
		return
	}
	httpx.JSON(w, 200, map[string]any{"valid": true, "sub": claims.Sub, "dom": claims.Dom, "room": claims.Rm, "exp": claims.Exp, "kid": claims.Kid})
}
