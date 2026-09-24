// Package hub : fan-out des événements Redis vers les flux SSE tenus par cette replica.
package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/flyc-io/flyc/queue/internal/room"
	"github.com/flyc-io/flyc/queue/internal/store"
)

// Msg envoyé à un abonné.
type Msg struct {
	Event string
	Data  any
}

// Status envoyé périodiquement à chaque visiteur en attente.
type Status struct {
	State    string  `json:"state"`
	Position int64   `json:"position"`  // 1 = prochain ; 0 = vague pas encore classée
	Ahead    int64   `json:"ahead"`     // tickets devant
	Pending  int64   `json:"pending"`   // taille de la file
	ETA      int64   `json:"eta_s"`     // estimation en secondes
	WaveIn   int64   `json:"wave_in_s"` // secondes avant classement de sa vague (0 si classée)
	Rate     float64 `json:"rate"`
}

type sub struct {
	ticket string
	ch     chan Msg
}

type roomHub struct {
	id     room.ID
	subs   map[*sub]struct{}
	cancel context.CancelFunc
}

// Hub gère un roomHub par file.
type Hub struct {
	st    *store.Store
	log   *slog.Logger
	mu    sync.Mutex
	rooms map[string]*roomHub
	gauge *prometheus.GaugeVec
}

func New(st *store.Store, reg prometheus.Registerer, log *slog.Logger) *Hub {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_sse_connections", Help: "Flux SSE ouverts sur cette replica"}, []string{"room"})
	reg.MustRegister(g)
	return &Hub{st: st, log: log, rooms: map[string]*roomHub{}, gauge: g}
}

// Subscribe enregistre un flux pour un ticket. Le canal est fermé par Unsubscribe.
func (h *Hub) Subscribe(id room.ID, ticket string) (<-chan Msg, func()) {
	h.mu.Lock()
	rh, ok := h.rooms[id.String()]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		rh = &roomHub{id: id, subs: map[*sub]struct{}{}, cancel: cancel}
		h.rooms[id.String()] = rh
		go h.pump(ctx, rh)
		go h.statusLoop(ctx, rh)
	}
	s := &sub{ticket: ticket, ch: make(chan Msg, 8)}
	rh.subs[s] = struct{}{}
	n := len(rh.subs)
	h.mu.Unlock()
	h.gauge.WithLabelValues(id.String()).Set(float64(n))
	return s.ch, func() {
		h.mu.Lock()
		delete(rh.subs, s)
		n := len(rh.subs)
		if n == 0 {
			rh.cancel()
			delete(h.rooms, id.String())
		}
		h.mu.Unlock()
		h.gauge.WithLabelValues(id.String()).Set(float64(n))
	}
}

// pump : relaie les événements Redis (admissions, config) aux abonnés concernés.
func (h *Hub) pump(ctx context.Context, rh *roomHub) {
	ps := h.st.Subscribe(ctx, rh.id)
	defer ps.Close()
	ch := ps.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			var ev store.Event
			if json.Unmarshal([]byte(m.Payload), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "admitted":
				set := make(map[string]struct{}, len(ev.Admitted))
				for _, t := range ev.Admitted {
					set[t] = struct{}{}
				}
				h.mu.Lock()
				for s := range rh.subs {
					if _, hit := set[s.ticket]; hit {
						h.send(s, Msg{Event: "admitted", Data: map[string]string{"ticket": s.ticket}})
					}
				}
				h.mu.Unlock()
			case "config", "flush":
				h.mu.Lock()
				for s := range rh.subs {
					h.send(s, Msg{Event: "refresh", Data: map[string]string{"reason": ev.Type}})
				}
				h.mu.Unlock()
			}
		}
	}
}

// statusLoop : toutes les 5 s, position de chaque abonné (ZRANK en pipeline).
func (h *Hub) statusLoop(ctx context.Context, rh *roomHub) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		subs := make([]*sub, 0, len(rh.subs))
		for s := range rh.subs {
			subs = append(subs, s)
		}
		h.mu.Unlock()
		if len(subs) == 0 {
			continue
		}
		cfg, _, err := h.st.Config(ctx, rh.id)
		if err != nil {
			continue
		}
		now := time.Now()
		pending, _, _, err := h.st.Counts(ctx, rh.id, now)
		if err != nil {
			continue
		}
		for i := 0; i < len(subs); i += 500 {
			end := i + 500
			if end > len(subs) {
				end = len(subs)
			}
			batch := subs[i:end]
			ids := make([]string, len(batch))
			for j, s := range batch {
				ids[j] = s.ticket
			}
			ranks, err := h.st.Ranks(ctx, rh.id, ids)
			if err != nil {
				continue
			}
			for j, s := range batch {
				st := BuildStatus(cfg, pending, ranks[j], 0, now)
				h.mu.Lock()
				h.send(s, Msg{Event: "status", Data: st})
				h.mu.Unlock()
			}
		}
	}
}

// BuildStatus calcule ce que voit un visiteur. rank = -1 si pas encore classé (ou admis).
// waveIn = secondes avant classement quand rank == -1 et que le ticket est encore en vague.
func BuildStatus(cfg room.Config, pending, rank, waveIn int64, now time.Time) Status {
	s := Status{State: string(cfg.State), Pending: pending, Rate: cfg.Rate}
	if rank >= 0 {
		s.Position = rank + 1
		s.Ahead = rank
		s.ETA = int64(float64(rank)/cfg.Rate) + 1
	} else {
		s.WaveIn = waveIn
		s.ETA = waveIn + int64(float64(pending)/cfg.Rate) + 1
	}
	return s
}

func (h *Hub) send(s *sub, m Msg) {
	select {
	case s.ch <- m:
	default: // abonné trop lent : on saute ce message, le suivant rattrapera
	}
}
