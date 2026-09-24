// Package store : toutes les opérations Redis de la file d'attente.
//
// Clés par file (préfixe {r:<tenant>:<room>}) :
//
//	cfg      HASH  configuration (voir room.Config)
//	wave:<n> SET   tickets arrivés pendant la fenêtre n, pas encore classés
//	pending  ZSET  tickets en attente, score = vague×10^7 + rang aléatoire
//	t:<id>   HASH  ticket (created, wave, status, score, dom, ret, pass, exp)
//	active   ZSET  pass émis, score = expiration
//	lock     STRING bail de l'admitter
//	lastwave STRING dernière vague classée
//	stats    HASH  compteurs
//	ev       pub/sub : admissions, tête de file, changements de config
//
// Clé globale : rooms (SET) — files connues, pour les boucles d'admission.
package store

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/flyc-io/flyc/queue/internal/room"
)

const (
	StatusWaiting  = "waiting"
	StatusAdmitted = "admitted"
	ticketTTL      = 24 * time.Hour
	RoomsKey       = "rooms"
)

// Store encapsule le client cluster.
type Store struct {
	R *redis.ClusterClient
}

func New(r *redis.ClusterClient) *Store { return &Store{R: r} }

// Ticket tel que stocké.
type Ticket struct {
	ID       string
	Created  int64
	Wave     int64
	Status   string
	Score    float64 // 0 tant que la vague n'est pas classée
	Dom      string
	Ret      string // chemin de retour (commence par /)
	Pass     string
	Exp      int64
	Admitted int64
}

// Event publié sur <key>:ev.
type Event struct {
	Type     string   `json:"type"`               // admitted | config | flush
	Admitted []string `json:"admitted,omitempty"` // ids de tickets
	Pending  int64    `json:"pending,omitempty"`
	Head     float64  `json:"head,omitempty"` // plus petit score en attente
	Rate     float64  `json:"rate,omitempty"`
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "t_" + hex.EncodeToString(b)
}

// Config lit la configuration ; found=false si la file n'a jamais été configurée.
func (s *Store) Config(ctx context.Context, id room.ID) (room.Config, bool, error) {
	h, err := s.R.HGetAll(ctx, id.Key("cfg")).Result()
	if err != nil {
		return room.Config{}, false, err
	}
	c, found := room.FromHash(h)
	return c, found, nil
}

// SetConfig écrit la configuration, enregistre la file et notifie.
func (s *Store) SetConfig(ctx context.Context, id room.ID, c room.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	pipe := s.R.Pipeline()
	pipe.HSet(ctx, id.Key("cfg"), c.ToHash())
	pipe.SAdd(ctx, RoomsKey, id.String())
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return s.Publish(ctx, id, Event{Type: "config", Rate: c.Rate})
}

// Rooms : files connues.
func (s *Store) Rooms(ctx context.Context) ([]string, error) {
	return s.R.SMembers(ctx, RoomsKey).Result()
}

// CreateTicket place un ticket dans la vague courante.
func (s *Store) CreateTicket(ctx context.Context, id room.ID, cfg room.Config, dom, ret string, now time.Time) (*Ticket, error) {
	t := &Ticket{ID: newID(), Created: now.Unix(), Wave: room.Wave(now, cfg.WaveSec), Status: StatusWaiting, Dom: dom, Ret: ret}
	pipe := s.R.Pipeline()
	pipe.HSet(ctx, id.Key("t:"+t.ID), map[string]any{
		"created": t.Created, "wave": t.Wave, "status": t.Status, "dom": dom, "ret": ret,
	})
	pipe.Expire(ctx, id.Key("t:"+t.ID), ticketTTL)
	pipe.SAdd(ctx, id.Key("wave:"+strconv.FormatInt(t.Wave, 10)), t.ID)
	pipe.Expire(ctx, id.Key("wave:"+strconv.FormatInt(t.Wave, 10)), 2*time.Hour)
	pipe.HIncrBy(ctx, id.Key("stats"), "tickets_total", 1)
	pipe.SAdd(ctx, RoomsKey, id.String())
	_, err := pipe.Exec(ctx)
	return t, err
}

// AdmitDirect : file en état open, le pass est émis sans attente.
func (s *Store) AdmitDirect(ctx context.Context, id room.ID, dom, ret, pass string, exp int64, now time.Time) (*Ticket, error) {
	t := &Ticket{ID: newID(), Created: now.Unix(), Status: StatusAdmitted, Dom: dom, Ret: ret, Pass: pass, Exp: exp, Admitted: now.Unix()}
	pipe := s.R.Pipeline()
	pipe.HSet(ctx, id.Key("t:"+t.ID), map[string]any{
		"created": t.Created, "status": t.Status, "dom": dom, "ret": ret, "pass": pass, "exp": exp, "admitted": t.Admitted,
	})
	pipe.Expire(ctx, id.Key("t:"+t.ID), ticketTTL)
	pipe.HIncrBy(ctx, id.Key("stats"), "tickets_total", 1)
	pipe.HIncrBy(ctx, id.Key("stats"), "admitted_total", 1)
	_, err := pipe.Exec(ctx)
	return t, err
}

// GetTicket lit un ticket ; nil si inconnu.
func (s *Store) GetTicket(ctx context.Context, id room.ID, tid string) (*Ticket, error) {
	h, err := s.R.HGetAll(ctx, id.Key("t:"+tid)).Result()
	if err != nil {
		return nil, err
	}
	if len(h) == 0 {
		return nil, nil
	}
	t := &Ticket{ID: tid, Status: h["status"], Dom: h["dom"], Ret: h["ret"], Pass: h["pass"]}
	t.Created, _ = strconv.ParseInt(h["created"], 10, 64)
	t.Wave, _ = strconv.ParseInt(h["wave"], 10, 64)
	t.Score, _ = strconv.ParseFloat(h["score"], 64)
	t.Exp, _ = strconv.ParseInt(h["exp"], 10, 64)
	t.Admitted, _ = strconv.ParseInt(h["admitted"], 10, 64)
	return t, nil
}

// LastWave : dernière vague classée (-1 si aucune).
func (s *Store) LastWave(ctx context.Context, id room.ID) (int64, error) {
	v, err := s.R.Get(ctx, id.Key("lastwave")).Result()
	if errors.Is(err, redis.Nil) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

func (s *Store) SetLastWave(ctx context.Context, id room.ID, n int64) error {
	return s.R.Set(ctx, id.Key("lastwave"), n, 0).Err()
}

// CloseWave vide la vague n, mélange ses tickets et les classe dans pending.
// Le mélange est fait ici (Fisher-Yates) : SPOP renvoie l'ensemble dans l'ordre d'insertion
// quand count dépasse la taille du set, ce n'est donc pas une source d'aléa fiable.
func (s *Store) CloseWave(ctx context.Context, id room.ID, n int64) (int64, error) {
	key := id.Key("wave:" + strconv.FormatInt(n, 10))
	var ids []string
	for {
		batch, err := s.R.SPopN(ctx, key, 1000).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return 0, err
		}
		if len(batch) == 0 {
			break
		}
		ids = append(ids, batch...)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	Shuffle(ids)
	for i := 0; i < len(ids); i += 1000 {
		end := i + 1000
		if end > len(ids) {
			end = len(ids)
		}
		pipe := s.R.Pipeline()
		zs := make([]redis.Z, 0, end-i)
		for j := i; j < end; j++ {
			score := room.WaveScore(n, int64(j))
			zs = append(zs, redis.Z{Score: score, Member: ids[j]})
			pipe.HSet(ctx, id.Key("t:"+ids[j]), "score", strconv.FormatFloat(score, 'f', 0, 64))
		}
		pipe.ZAdd(ctx, id.Key("pending"), zs...)
		if _, err := pipe.Exec(ctx); err != nil {
			return int64(i), err
		}
	}
	s.R.Del(ctx, key)
	return int64(len(ids)), nil
}

// Shuffle : permutation uniforme (Fisher-Yates) avec de l'aléa cryptographique.
func Shuffle(ids []string) {
	for i := len(ids) - 1; i > 0; i-- {
		j := int(randUint64() % uint64(i+1))
		ids[i], ids[j] = ids[j], ids[i]
	}
}

func randUint64() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// StrayWaves liste les vagues encore présentes qui ne sont ni la vague courante ni la précédente
// (numérotation changée après un changement de wave_s, ou admitter absent longtemps).
func (s *Store) StrayWaves(ctx context.Context, id room.ID, cur int64) ([]int64, error) {
	pattern := id.Key("wave:*")
	var out []int64
	err := s.R.ForEachMaster(ctx, func(ctx context.Context, c *redis.Client) error {
		iter := c.Scan(ctx, 0, pattern, 200).Iterator()
		for iter.Next(ctx) {
			k := iter.Val()
			n, err := strconv.ParseInt(k[strings.LastIndex(k, ":")+1:], 10, 64)
			if err != nil || n == cur || n == cur-1 {
				continue
			}
			out = append(out, n)
		}
		return iter.Err()
	})
	return out, err
}

// Counts : en attente, actifs (après purge des pass expirés), tête de file.
func (s *Store) Counts(ctx context.Context, id room.ID, now time.Time) (pending, active int64, head float64, err error) {
	pipe := s.R.Pipeline()
	pipe.ZRemRangeByScore(ctx, id.Key("active"), "-inf", strconv.FormatInt(now.Unix(), 10))
	pc := pipe.ZCard(ctx, id.Key("pending"))
	ac := pipe.ZCard(ctx, id.Key("active"))
	hc := pipe.ZRangeWithScores(ctx, id.Key("pending"), 0, 0)
	if _, err = pipe.Exec(ctx); err != nil {
		return
	}
	pending, active = pc.Val(), ac.Val()
	if h := hc.Val(); len(h) > 0 {
		head = h[0].Score
	}
	return
}

// PopPending retire jusqu'à k tickets en tête de file.
func (s *Store) PopPending(ctx context.Context, id room.ID, k int64) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	zs, err := s.R.ZPopMin(ctx, id.Key("pending"), k).Result()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zs))
	for _, z := range zs {
		out = append(out, z.Member.(string))
	}
	return out, nil
}

// MarkAdmitted enregistre le pass d'un ticket et le compte parmi les actifs.
func (s *Store) MarkAdmitted(ctx context.Context, id room.ID, tid, pass string, exp, now int64) error {
	pipe := s.R.Pipeline()
	pipe.HSet(ctx, id.Key("t:"+tid), map[string]any{"status": StatusAdmitted, "pass": pass, "exp": exp, "admitted": now})
	pipe.ZAdd(ctx, id.Key("active"), redis.Z{Score: float64(exp), Member: tid})
	pipe.HIncrBy(ctx, id.Key("stats"), "admitted_total", 1)
	_, err := pipe.Exec(ctx)
	return err
}

// Requeue remet un ticket classé mais absent de pending (perdu entre pop et admission) à sa place.
func (s *Store) Requeue(ctx context.Context, id room.ID, t *Ticket) error {
	if t.Score == 0 {
		return nil
	}
	return s.R.ZAdd(ctx, id.Key("pending"), redis.Z{Score: t.Score, Member: t.ID}).Err()
}

// Rank : position (0 = tête) dans pending, -1 si absent.
func (s *Store) Rank(ctx context.Context, id room.ID, tid string) (int64, error) {
	r, err := s.R.ZRank(ctx, id.Key("pending"), tid).Result()
	if errors.Is(err, redis.Nil) {
		return -1, nil
	}
	return r, err
}

// Ranks : positions de plusieurs tickets en un pipeline (-1 = absent).
func (s *Store) Ranks(ctx context.Context, id room.ID, tids []string) ([]int64, error) {
	pipe := s.R.Pipeline()
	cmds := make([]*redis.IntCmd, len(tids))
	for i, tid := range tids {
		cmds[i] = pipe.ZRank(ctx, id.Key("pending"), tid)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := make([]int64, len(tids))
	for i, c := range cmds {
		if c.Err() != nil {
			out[i] = -1
		} else {
			out[i] = c.Val()
		}
	}
	return out, nil
}

// WaveSize : tickets encore dans la vague n (non classés).
func (s *Store) WaveSize(ctx context.Context, id room.ID, n int64) (int64, error) {
	return s.R.SCard(ctx, id.Key("wave:"+strconv.FormatInt(n, 10))).Result()
}

// Lock : bail de l'admitter. Acquire renvoie true si ce process détient la file.
func (s *Store) Acquire(ctx context.Context, id room.ID, owner string, ttl time.Duration) (bool, error) {
	ok, err := s.R.SetNX(ctx, id.Key("lock"), owner, ttl).Result()
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	cur, err := s.R.Get(ctx, id.Key("lock")).Result()
	if err != nil {
		return false, err
	}
	if cur == owner {
		return true, s.R.Expire(ctx, id.Key("lock"), ttl).Err()
	}
	return false, nil
}

var releaseScript = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) end return 0`)

func (s *Store) Release(ctx context.Context, id room.ID, owner string) {
	_, _ = releaseScript.Run(ctx, s.R, []string{id.Key("lock")}, owner).Result()
}

// Publish / Subscribe sur le canal d'événements de la file.
func (s *Store) Publish(ctx context.Context, id room.ID, ev Event) error {
	b, _ := json.Marshal(ev)
	return s.R.Publish(ctx, id.Key("ev"), b).Err()
}

func (s *Store) Subscribe(ctx context.Context, id room.ID) *redis.PubSub {
	return s.R.Subscribe(ctx, id.Key("ev"))
}

// Stats : compteurs cumulés.
func (s *Store) Stats(ctx context.Context, id room.ID) (map[string]string, error) {
	return s.R.HGetAll(ctx, id.Key("stats")).Result()
}

// Flush vide la file (tickets en attente, vagues) sans toucher aux pass déjà émis.
func (s *Store) Flush(ctx context.Context, id room.ID, now time.Time, waveSec int64) error {
	cur := room.Wave(now, waveSec)
	pipe := s.R.Pipeline()
	pipe.Del(ctx, id.Key("pending"))
	for n := cur - 200; n <= cur+1; n++ {
		pipe.Del(ctx, id.Key("wave:"+strconv.FormatInt(n, 10)))
	}
	pipe.Set(ctx, id.Key("lastwave"), cur-1, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return s.Publish(ctx, id, Event{Type: "flush"})
}
