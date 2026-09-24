// Package admitter : une boucle par file, tenue par une seule replica (bail Redis).
//
// À chaque seconde : ferme les vagues écoulées (mélange), purge les pass expirés,
// admet k = min(débit accumulé, places actives libres, en attente) tickets en tête de file,
// signe leur pass, publie l'événement.
package admitter

import (
	"context"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flyc-io/flyc/internal/pass"
	"github.com/flyc-io/flyc/queue/internal/keys"
	"github.com/flyc-io/flyc/queue/internal/room"
	"github.com/flyc-io/flyc/queue/internal/store"
)

const (
	lockTTL   = 5 * time.Second
	tick      = time.Second
	waveGrace = time.Second // marge après la fin d'une fenêtre avant de la classer
)

// Metrics exposées par le manager.
type Metrics struct {
	Admitted  *prometheus.CounterVec
	Pending   *prometheus.GaugeVec
	Active    *prometheus.GaugeVec
	Leader    *prometheus.GaugeVec
	LockAge   *prometheus.GaugeVec
	WaveClose *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Admitted:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flyc_queue_admitted_total", Help: "Tickets admis"}, []string{"room"}),
		Pending:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_pending", Help: "Tickets en attente (classés)"}, []string{"room"}),
		Active:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_active", Help: "Pass valides"}, []string{"room"}),
		Leader:    prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_admitter_leader", Help: "1 si cette replica tient la file"}, []string{"room"}),
		LockAge:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_admitter_tick_age_seconds", Help: "Âge du dernier tick"}, []string{"room"}),
		WaveClose: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "flyc_queue_waves_closed_total", Help: "Vagues classées"}, []string{"room"}),
	}
	reg.MustRegister(m.Admitted, m.Pending, m.Active, m.Leader, m.LockAge, m.WaveClose)
	return m
}

// Manager découvre les files et lance une boucle par file.
type Manager struct {
	st    *store.Store
	keys  *keys.Client
	log   *slog.Logger
	m     *Metrics
	owner string
	mu    sync.Mutex
	loops map[string]context.CancelFunc
}

func NewManager(st *store.Store, k *keys.Client, m *Metrics, log *slog.Logger) *Manager {
	host, _ := os.Hostname()
	return &Manager{st: st, keys: k, log: log, m: m, owner: host + "/" + store.RoomsKey + time.Now().Format("150405"), loops: map[string]context.CancelFunc{}}
}

// Run : rafraîchit la liste des files toutes les 5 s.
func (mg *Manager) Run(ctx context.Context) {
	for {
		rooms, err := mg.st.Rooms(ctx)
		if err != nil {
			mg.log.Warn("liste des files", "err", err)
		}
		mg.mu.Lock()
		for _, r := range rooms {
			if _, ok := mg.loops[r]; ok {
				continue
			}
			id, err := room.Parse(r)
			if err != nil {
				continue
			}
			lctx, cancel := context.WithCancel(ctx)
			mg.loops[r] = cancel
			go mg.loop(lctx, id)
		}
		mg.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// loop : tente le bail ; tant qu'il est tenu, exécute un tick par seconde.
func (mg *Manager) loop(ctx context.Context, id room.ID) {
	log := mg.log.With("room", id.String())
	var tokens float64
	last := time.Now()
	leader := false
	for {
		select {
		case <-ctx.Done():
			if leader {
				mg.st.Release(context.Background(), id, mg.owner)
			}
			return
		case <-time.After(tick):
		}
		ok, err := mg.st.Acquire(ctx, id, mg.owner, lockTTL)
		if err != nil {
			log.Warn("bail", "err", err)
			continue
		}
		if !ok {
			if leader {
				log.Info("bail perdu")
			}
			leader = false
			mg.m.Leader.WithLabelValues(id.String()).Set(0)
			tokens = 0
			last = time.Now()
			continue
		}
		if !leader {
			log.Info("admitter actif")
			leader = true
			last = time.Now()
		}
		mg.m.Leader.WithLabelValues(id.String()).Set(1)
		now := time.Now()
		dt := now.Sub(last).Seconds()
		last = now
		if err := mg.tickOnce(ctx, id, now, dt, &tokens, log); err != nil {
			log.Warn("tick", "err", err)
		}
	}
}

func (mg *Manager) tickOnce(ctx context.Context, id room.ID, now time.Time, dt float64, tokens *float64, log *slog.Logger) error {
	cfg, _, err := mg.st.Config(ctx, id)
	if err != nil {
		return err
	}
	// 1. Fermeture des vagues écoulées.
	cur := room.Wave(now, cfg.WaveSec)
	lastWave, err := mg.st.LastWave(ctx, id)
	if err != nil {
		return err
	}
	// Compteur absent, trop ancien, ou issu d'une autre numérotation (changement de wave_s) : on repart d'ici.
	if lastWave < 0 || lastWave < cur-200 || lastWave >= cur {
		lastWave = cur - 2
	}
	for n := lastWave + 1; n < cur; n++ {
		if time.Unix((n+1)*cfg.WaveSec, 0).Add(waveGrace).After(now) {
			break
		}
		cnt, err := mg.st.CloseWave(ctx, id, n)
		if err != nil {
			return err
		}
		if cnt > 0 {
			mg.m.WaveClose.WithLabelValues(id.String()).Inc()
			log.Info("vague classée", "wave", n, "tickets", cnt)
		}
		if err := mg.st.SetLastWave(ctx, id, n); err != nil {
			return err
		}
	}
	// 1b. Toutes les 30 s : vagues orphelines (numérotation changée, admitter absent longtemps).
	if now.Unix()%30 == 0 {
		strays, err := mg.st.StrayWaves(ctx, id, cur)
		if err != nil {
			log.Warn("balayage des vagues", "err", err)
		}
		for _, n := range strays {
			cnt, err := mg.st.CloseWave(ctx, id, n)
			if err != nil {
				return err
			}
			if cnt > 0 {
				mg.m.WaveClose.WithLabelValues(id.String()).Inc()
				log.Info("vague orpheline classée", "wave", n, "tickets", cnt)
			}
		}
	}
	// 2. Compteurs (purge des pass expirés incluse).
	pending, active, head, err := mg.st.Counts(ctx, id, now)
	if err != nil {
		return err
	}
	mg.m.Pending.WithLabelValues(id.String()).Set(float64(pending))
	mg.m.Active.WithLabelValues(id.String()).Set(float64(active))
	mg.m.LockAge.WithLabelValues(id.String()).Set(0)
	if cfg.State != room.Queue || pending == 0 {
		*tokens = math.Min(*tokens+cfg.Rate*dt, cfg.Rate*5)
		return nil
	}
	// 3. Admission au débit configuré (seau à jetons, rafale max 5 s).
	*tokens = math.Min(*tokens+cfg.Rate*dt, cfg.Rate*5)
	k := int64(math.Floor(*tokens))
	if cfg.MaxActive > 0 && cfg.MaxActive-active < k {
		k = cfg.MaxActive - active
	}
	if k > pending {
		k = pending
	}
	if k <= 0 {
		return nil
	}
	kid, priv := mg.keys.Current()
	if kid == "" {
		log.Warn("aucune clé de signature : admission suspendue")
		return nil
	}
	ids, err := mg.st.PopPending(ctx, id, k)
	if err != nil {
		return err
	}
	admitted := make([]string, 0, len(ids))
	for _, tid := range ids {
		t, err := mg.st.GetTicket(ctx, id, tid)
		if err != nil || t == nil {
			continue
		}
		tok, err := pass.Mint(priv, kid, pass.Claims{Sub: tid, Tenant: id.Tenant, Dom: t.Dom, Room: id.String(), TTL: cfg.PassTTL})
		if err != nil {
			log.Error("signature du pass", "err", err)
			_ = mg.st.Requeue(ctx, id, t)
			continue
		}
		exp := now.Add(cfg.PassTTL).Unix()
		if err := mg.st.MarkAdmitted(ctx, id, tid, tok, exp, now.Unix()); err != nil {
			_ = mg.st.Requeue(ctx, id, t)
			return err
		}
		admitted = append(admitted, tid)
	}
	*tokens -= float64(len(admitted))
	mg.m.Admitted.WithLabelValues(id.String()).Add(float64(len(admitted)))
	if len(admitted) > 0 {
		return mg.st.Publish(ctx, id, store.Event{Type: "admitted", Admitted: admitted, Pending: pending - int64(len(admitted)), Head: head, Rate: cfg.Rate})
	}
	return nil
}
