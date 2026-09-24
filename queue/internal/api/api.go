// Package api : API publique de la salle d'attente (/q/...) et API interne (/internal/...).
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flyc-io/flyc/internal/httpx"
	"github.com/flyc-io/flyc/internal/pass"
	"github.com/flyc-io/flyc/queue/internal/hub"
	"github.com/flyc-io/flyc/queue/internal/keys"
	"github.com/flyc-io/flyc/queue/internal/room"
	"github.com/flyc-io/flyc/queue/internal/store"
)

// Options de l'API.
type Options struct {
	InternalToken    string
	CookieSecret     []byte // HMAC du cookie flyc_ticket
	TurnstileSecret  string // vide = pas de captcha
	TurnstileSiteKey string
	WaitHost         string
}

type API struct {
	st        *store.Store
	hub       *hub.Hub
	keys      *keys.Client
	opt       Options
	log       *slog.Logger
	mTickets  *prometheus.CounterVec
	mDegraded prometheus.Counter
}

func New(st *store.Store, h *hub.Hub, k *keys.Client, opt Options, reg prometheus.Registerer, log *slog.Logger) *API {
	a := &API{st: st, hub: h, keys: k, opt: opt, log: log,
		mTickets:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flyc_queue_tickets_total", Help: "Tickets créés"}, []string{"room", "result"}),
		mDegraded: prometheus.NewCounter(prometheus.CounterOpts{Name: "flyc_queue_degraded_total", Help: "Flux SSE refusés en mode dégradé"}),
	}
	reg.MustRegister(a.mTickets, a.mDegraded)
	return a
}

// Register pose les routes sur le mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /q/{room}/config", a.config)
	mux.HandleFunc("POST /q/{room}/ticket", a.ticket)
	mux.HandleFunc("GET /q/{room}/status", a.status)
	mux.HandleFunc("GET /q/{room}/events", a.events)
	mux.HandleFunc("GET /q/{room}/go", a.goTo)
	mux.HandleFunc("GET /q/{room}/pass", a.passJSON)
	mux.HandleFunc("PUT /internal/rooms/{room}", a.auth(a.putRoom))
	mux.HandleFunc("GET /internal/rooms/{room}", a.auth(a.getRoom))
	mux.HandleFunc("POST /internal/rooms/{room}/flush", a.auth(a.flushRoom))
}

// ---------- helpers ----------

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.opt.InternalToken == "" || r.Header.Get("Authorization") != "Bearer "+a.opt.InternalToken {
			httpx.Error(w, http.StatusUnauthorized, "unauthorized", "jeton interne invalide")
			return
		}
		next(w, r)
	}
}

func (a *API) roomOf(w http.ResponseWriter, r *http.Request) (room.ID, room.Config, bool) {
	id, err := room.Parse(r.PathValue("room"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "unknown_room", "file inconnue")
		return id, room.Config{}, false
	}
	cfg, found, err := a.st.Config(r.Context(), id)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "store", "file d'attente indisponible")
		return id, cfg, false
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "unknown_room", "file non configurée")
		return id, cfg, false
	}
	return id, cfg, true
}

// signTicket : cookie flyc_ticket = <id>.<hmac(room|id)>
func (a *API) signTicket(id room.ID, tid string) string {
	m := hmac.New(sha256.New, a.opt.CookieSecret)
	m.Write([]byte(id.String() + "|" + tid))
	return tid + "." + hex.EncodeToString(m.Sum(nil))[:32]
}

func (a *API) verifyTicket(id room.ID, v string) (string, bool) {
	tid, sig, ok := strings.Cut(v, ".")
	if !ok || tid == "" {
		return "", false
	}
	want := strings.TrimPrefix(a.signTicket(id, tid), tid+".")
	return tid, hmac.Equal([]byte(sig), []byte(want))
}

func (a *API) setTicketCookie(w http.ResponseWriter, id room.ID, tid string) {
	http.SetCookie(w, &http.Cookie{Name: "flyc_ticket_" + id.Tenant + "_" + id.Room, Value: a.signTicket(id, tid), Path: "/q/" + id.String(), MaxAge: 6 * 3600, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// ticketFromRequest : paramètre t (JS) ou cookie signé.
func (a *API) ticketFromRequest(r *http.Request, id room.ID) string {
	if t := r.URL.Query().Get("t"); t != "" {
		return t
	}
	if c, err := r.Cookie("flyc_ticket_" + id.Tenant + "_" + id.Room); err == nil {
		if tid, ok := a.verifyTicket(id, c.Value); ok {
			return tid
		}
	}
	return ""
}

// normalizeReturn accepte un chemin relatif ou une URL absolue sur le domaine protégé.
func normalizeReturn(raw, dom string) string {
	if raw == "" {
		return "/"
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil {
		raw = string(b)
	} else if b, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		raw = string(b)
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return raw
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" && strings.EqualFold(u.Hostname(), dom) {
		p := u.EscapedPath()
		if p == "" {
			p = "/"
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return p
	}
	return "/"
}

func goURL(dom, ret, tok string) string {
	return "https://" + dom + "/__flyc/go?p=" + url.QueryEscape(tok) + "&r=" + url.QueryEscape(base64.StdEncoding.EncodeToString([]byte(ret)))
}

// ---------- routes publiques ----------

func (a *API) config(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	httpx.JSON(w, 200, map[string]any{"room": id.String(), "state": cfg.State, "wave_s": cfg.WaveSec, "title": cfg.Title, "opens_at": cfg.OpensAt, "turnstile_sitekey": a.opt.TurnstileSiteKey, "theme": cfg.Theme})
}

type ticketReq struct {
	H         string `json:"h"`
	R         string `json:"r"`
	Turnstile string `json:"turnstile"`
}

func (a *API) ticket(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	var req ticketReq
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	dom := strings.ToLower(strings.TrimSpace(req.H))
	if !cfg.AllowsDomain(dom) {
		a.mTickets.WithLabelValues(id.String(), "bad_domain").Inc()
		httpx.Error(w, http.StatusBadRequest, "bad_domain", "ce domaine n'est pas protégé par cette file")
		return
	}
	ret := normalizeReturn(req.R, dom)
	now := time.Now()

	// File fermée : personne ne passe, même avec un ticket déjà admis (sinon boucle avec la redirection du LB).
	if cfg.State == room.Closed {
		httpx.JSON(w, 200, map[string]any{"state": "closed", "opens_at": cfg.OpensAt, "title": cfg.Title, "theme": cfg.Theme})
		return
	}
	// Ticket existant (rafraîchissement, onglet rouvert) : on le rend tel quel.
	if c, err := r.Cookie("flyc_ticket_" + id.Tenant + "_" + id.Room); err == nil {
		if tid, ok := a.verifyTicket(id, c.Value); ok {
			if t, _ := a.st.GetTicket(r.Context(), id, tid); t != nil && (t.Status == store.StatusWaiting || t.Exp > now.Unix()) {
				a.respondTicket(w, r, id, cfg, t, now)
				return
			}
		}
	}
	if a.opt.TurnstileSecret != "" && cfg.State == room.Queue {
		if !a.verifyTurnstile(r.Context(), req.Turnstile, r.Header.Get("X-Real-IP")) {
			a.mTickets.WithLabelValues(id.String(), "captcha").Inc()
			httpx.Error(w, http.StatusForbidden, "captcha", "vérification anti-robot échouée")
			return
		}
	}
	if cfg.State == room.Open {
		kid, priv := a.keys.Current()
		if kid == "" {
			httpx.Error(w, http.StatusServiceUnavailable, "no_key", "signature indisponible")
			return
		}
		tok, err := pass.Mint(priv, kid, pass.Claims{Tenant: id.Tenant, Dom: dom, Room: id.String(), TTL: cfg.PassTTL})
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "sign", "signature impossible")
			return
		}
		t, err := a.st.AdmitDirect(r.Context(), id, dom, ret, tok, now.Add(cfg.PassTTL).Unix(), now)
		if err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "store", "file d'attente indisponible")
			return
		}
		a.mTickets.WithLabelValues(id.String(), "open").Inc()
		a.setTicketCookie(w, id, t.ID)
		httpx.JSON(w, 200, map[string]any{"ticket": t.ID, "state": "admitted", "go": "/q/" + id.String() + "/go?t=" + t.ID})
		return
	}
	t, err := a.st.CreateTicket(r.Context(), id, cfg, dom, ret, now)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "store", "file d'attente indisponible")
		return
	}
	a.mTickets.WithLabelValues(id.String(), "created").Inc()
	a.setTicketCookie(w, id, t.ID)
	a.respondTicket(w, r, id, cfg, t, now)
}

func (a *API) respondTicket(w http.ResponseWriter, r *http.Request, id room.ID, cfg room.Config, t *store.Ticket, now time.Time) {
	if t.Status == store.StatusAdmitted {
		httpx.JSON(w, 200, map[string]any{"ticket": t.ID, "state": "admitted", "go": "/q/" + id.String() + "/go?t=" + t.ID})
		return
	}
	st, _ := a.statusOf(r.Context(), id, cfg, t, now)
	httpx.JSON(w, 200, map[string]any{"ticket": t.ID, "state": "waiting", "status": st, "title": cfg.Title, "theme": cfg.Theme})
}

func (a *API) statusOf(ctx context.Context, id room.ID, cfg room.Config, t *store.Ticket, now time.Time) (hub.Status, error) {
	pending, _, _, err := a.st.Counts(ctx, id, now)
	if err != nil {
		return hub.Status{}, err
	}
	rank, err := a.st.Rank(ctx, id, t.ID)
	if err != nil {
		return hub.Status{}, err
	}
	var waveIn int64
	if rank < 0 {
		if t.Score > 0 { // classé mais absent de pending : remis à sa place
			_ = a.st.Requeue(ctx, id, t)
			rank, _ = a.st.Rank(ctx, id, t.ID)
		} else {
			waveIn = (t.Wave+1)*cfg.WaveSec + 1 - now.Unix()
			if waveIn < 0 {
				waveIn = 0
			}
		}
	}
	return hub.BuildStatus(cfg, pending, rank, waveIn, now), nil
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	tid := a.ticketFromRequest(r, id)
	t, _ := a.st.GetTicket(r.Context(), id, tid)
	if t == nil {
		httpx.Error(w, http.StatusNotFound, "unknown_ticket", "ticket inconnu ou expiré")
		return
	}
	if cfg.State == room.Closed {
		httpx.JSON(w, 200, map[string]any{"state": "closed", "opens_at": cfg.OpensAt})
		return
	}
	if t.Status == store.StatusAdmitted {
		httpx.JSON(w, 200, map[string]any{"state": "admitted", "go": "/q/" + id.String() + "/go?t=" + t.ID})
		return
	}
	st, err := a.statusOf(r.Context(), id, cfg, t, time.Now())
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "store", "file d'attente indisponible")
		return
	}
	httpx.JSON(w, 200, map[string]any{"state": "waiting", "status": st, "poll_s": 5})
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	if r.Header.Get("X-Flyc-Degraded") != "" { // LB proche de son plafond : polling long
		a.mDegraded.Inc()
		w.Header().Set("Retry-After", "15")
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"error": "degraded", "poll_s": 15})
		return
	}
	tid := a.ticketFromRequest(r, id)
	t, _ := a.st.GetTicket(r.Context(), id, tid)
	if t == nil {
		httpx.Error(w, http.StatusNotFound, "unknown_ticket", "ticket inconnu ou expiré")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, http.StatusInternalServerError, "stream", "flux impossible")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	write := func(ev string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
		fl.Flush()
	}
	if cfg.State == room.Closed {
		write("closed", map[string]any{"opens_at": cfg.OpensAt})
		return
	}
	if t.Status == store.StatusAdmitted {
		write("admitted", map[string]string{"ticket": t.ID, "go": "/q/" + id.String() + "/go?t=" + t.ID})
		return
	}
	if st, err := a.statusOf(r.Context(), id, cfg, t, time.Now()); err == nil {
		write("status", st)
	}
	ch, unsub := a.hub.Subscribe(id, t.ID)
	defer unsub()
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			fmt.Fprint(w, ": hb\n\n")
			fl.Flush()
		case m := <-ch:
			if m.Event == "admitted" {
				write("admitted", map[string]string{"ticket": t.ID, "go": "/q/" + id.String() + "/go?t=" + t.ID})
				return
			}
			write(m.Event, m.Data)
		}
	}
}

// goTo : le ticket est admis → redirection vers le domaine protégé avec le pass (jamais exposé au JS).
func (a *API) goTo(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	tid := a.ticketFromRequest(r, id)
	t, _ := a.st.GetTicket(r.Context(), id, tid)
	if t == nil || t.Status != store.StatusAdmitted || t.Pass == "" {
		httpx.Error(w, http.StatusForbidden, "not_admitted", "ce ticket n'est pas encore admis")
		return
	}
	if t.Exp <= time.Now().Unix() {
		httpx.Error(w, http.StatusGone, "expired", "le pass a expiré, reprenez un ticket")
		return
	}
	if cfg.State == room.Closed {
		http.Redirect(w, r, "/w/"+id.String()+"?h="+url.QueryEscape(t.Dom)+"&closed=1", http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, goURL(t.Dom, t.Ret, t.Pass), http.StatusFound)
}

// passJSON : mode iframe / JS. Le pass est rendu à la page d'attente, qui le transmet à la page hôte
// par postMessage ; le domaine cible a été validé à la prise de ticket.
func (a *API) passJSON(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	tid := a.ticketFromRequest(r, id)
	t, _ := a.st.GetTicket(r.Context(), id, tid)
	if t == nil || t.Status != store.StatusAdmitted || t.Pass == "" {
		httpx.Error(w, http.StatusForbidden, "not_admitted", "ce ticket n'est pas encore admis")
		return
	}
	if t.Exp <= time.Now().Unix() || cfg.State == room.Closed {
		httpx.Error(w, http.StatusGone, "expired", "le pass a expiré ou la file est fermée")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, map[string]any{"pass": t.Pass, "exp": t.Exp, "dom": t.Dom, "room": id.String()})
}

// ---------- routes internes ----------

func (a *API) putRoom(w http.ResponseWriter, r *http.Request) {
	id, err := room.Parse(r.PathValue("room"))
	if err != nil {
		httpx.Error(w, 400, "bad_room", err.Error())
		return
	}
	cfg := room.Defaults()
	var in struct {
		room.Config
		PassTTLSec int64 `json:"pass_ttl_s"`
	}
	in.Config = cfg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&in); err != nil {
		httpx.Error(w, 400, "bad_json", err.Error())
		return
	}
	cfg = in.Config
	if in.PassTTLSec > 0 {
		cfg.PassTTL = time.Duration(in.PassTTLSec) * time.Second
	}
	if err := cfg.Validate(); err != nil {
		httpx.Error(w, 400, "invalid", err.Error())
		return
	}
	if err := a.st.SetConfig(r.Context(), id, cfg); err != nil {
		httpx.Error(w, 503, "store", err.Error())
		return
	}
	a.log.Info("file configurée", "room", id.String(), "state", cfg.State, "rate", cfg.Rate, "wave_s", cfg.WaveSec)
	httpx.JSON(w, 200, cfg)
}

func (a *API) getRoom(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	now := time.Now()
	pending, active, head, _ := a.st.Counts(r.Context(), id, now)
	inWave, _ := a.st.WaveSize(r.Context(), id, room.Wave(now, cfg.WaveSec))
	stats, _ := a.st.Stats(r.Context(), id)
	httpx.JSON(w, 200, map[string]any{"room": id.String(), "config": cfg, "pending": pending, "active": active, "in_wave": inWave, "head": head, "stats": stats})
}

func (a *API) flushRoom(w http.ResponseWriter, r *http.Request) {
	id, cfg, ok := a.roomOf(w, r)
	if !ok {
		return
	}
	if err := a.st.Flush(r.Context(), id, time.Now(), cfg.WaveSec); err != nil {
		httpx.Error(w, 503, "store", err.Error())
		return
	}
	a.log.Warn("file vidée", "room", id.String())
	httpx.JSON(w, 200, map[string]string{"status": "flushed"})
}

// verifyTurnstile appelle l'API siteverify de Cloudflare.
func (a *API) verifyTurnstile(ctx context.Context, token, ip string) bool {
	if token == "" {
		return false
	}
	form := url.Values{"secret": {a.opt.TurnstileSecret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		a.log.Warn("turnstile injoignable", "err", err)
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Success
}
