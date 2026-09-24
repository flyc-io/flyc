// Package room : identifiants et configuration des files d'attente.
package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ID d'une file tel qu'il circule dans HAProxy et les URL : "<tenant>.<room>".
type ID struct {
	Tenant string
	Room   string
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Parse valide "tenant.room".
func Parse(s string) (ID, error) {
	t, r, ok := strings.Cut(s, ".")
	if !ok || !slugRe.MatchString(t) || !slugRe.MatchString(r) {
		return ID{}, fmt.Errorf("identifiant de file invalide : %q", s)
	}
	return ID{Tenant: t, Room: r}, nil
}

func (id ID) String() string { return id.Tenant + "." + id.Room }

// Key préfixe toutes les clés Redis d'une file avec un hash tag : même slot, scripts et
// pipelines multi-clés possibles en cluster.
func (id ID) Key(suffix string) string {
	return "{r:" + id.Tenant + ":" + id.Room + "}:" + suffix
}

// State d'une file, aligné sur states.map côté HAProxy.
type State string

const (
	Open   State = "open"
	Queue  State = "queue"
	Closed State = "closed"
)

// Config d'une file. Stockée dans <key>:cfg (HASH), écrite par flyc-control (M3) ou l'API interne.
type Config struct {
	State     State             `json:"state"`
	Rate      float64           `json:"rate"`       // admissions par seconde
	MaxActive int64             `json:"max_active"` // pass valides simultanés (0 = illimité)
	WaveSec   int64             `json:"wave_s"`     // fenêtre d'arrivée mélangée
	PassTTL   time.Duration     `json:"pass_ttl"`   // validité du pass
	Domains   []string          `json:"domains"`    // domaines autorisés à demander un ticket
	OpensAt   int64             `json:"opens_at"`   // unix, informatif quand closed
	Title     string            `json:"title"`      // affiché sur la page d'attente
	Theme     map[string]string `json:"theme"`      // accent, logo_url, message, lang
}

// Defaults raisonnables pour une file créée sans configuration.
func Defaults() Config {
	return Config{State: Queue, Rate: 10, MaxActive: 0, WaveSec: 30, PassTTL: 20 * time.Minute}
}

// Validate borne les valeurs.
func (c *Config) Validate() error {
	switch c.State {
	case Open, Queue, Closed:
	case "":
		c.State = Queue
	default:
		return errors.New("state doit être open, queue ou closed")
	}
	if c.Rate <= 0 {
		c.Rate = 10
	}
	if c.Rate > 100000 {
		return errors.New("rate trop élevé")
	}
	if c.WaveSec < 1 {
		c.WaveSec = 30
	}
	if c.WaveSec > 600 {
		return errors.New("wave_s au maximum 600")
	}
	if c.PassTTL < time.Minute {
		c.PassTTL = 20 * time.Minute
	}
	if c.PassTTL > 24*time.Hour {
		return errors.New("pass_ttl au maximum 24h")
	}
	for i, d := range c.Domains {
		c.Domains[i] = strings.ToLower(strings.TrimSpace(d))
	}
	return nil
}

// AllowsDomain : le domaine peut-il demander un ticket pour cette file ?
func (c Config) AllowsDomain(h string) bool {
	h = strings.ToLower(h)
	for _, d := range c.Domains {
		if d == h {
			return true
		}
	}
	return false
}

// ToHash / FromHash : représentation Redis (HASH de chaînes).
func (c Config) ToHash() map[string]any {
	return map[string]any{
		"state": string(c.State), "rate": strconv.FormatFloat(c.Rate, 'f', -1, 64),
		"max_active": strconv.FormatInt(c.MaxActive, 10), "wave_s": strconv.FormatInt(c.WaveSec, 10),
		"pass_ttl": strconv.FormatInt(int64(c.PassTTL.Seconds()), 10), "domains": strings.Join(c.Domains, ","),
		"opens_at": strconv.FormatInt(c.OpensAt, 10), "title": c.Title, "theme": themeJSON(c.Theme),
	}
}

func themeJSON(t map[string]string) string {
	if len(t) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(t)
	return string(b)
}

func FromHash(h map[string]string) (Config, bool) {
	if len(h) == 0 {
		return Defaults(), false
	}
	c := Defaults()
	c.State = State(h["state"])
	if v, err := strconv.ParseFloat(h["rate"], 64); err == nil {
		c.Rate = v
	}
	if v, err := strconv.ParseInt(h["max_active"], 10, 64); err == nil {
		c.MaxActive = v
	}
	if v, err := strconv.ParseInt(h["wave_s"], 10, 64); err == nil {
		c.WaveSec = v
	}
	if v, err := strconv.ParseInt(h["pass_ttl"], 10, 64); err == nil {
		c.PassTTL = time.Duration(v) * time.Second
	}
	if h["domains"] != "" {
		c.Domains = strings.Split(h["domains"], ",")
	}
	if v, err := strconv.ParseInt(h["opens_at"], 10, 64); err == nil {
		c.OpensAt = v
	}
	c.Title = h["title"]
	if h["theme"] != "" {
		_ = json.Unmarshal([]byte(h["theme"]), &c.Theme)
	}
	_ = c.Validate()
	return c, true
}

// Wave : numéro de fenêtre d'arrivée pour un instant donné.
func Wave(now time.Time, waveSec int64) int64 { return now.Unix() / waveSec }

// WaveScore : score dans pending = vague × 10^7 + rang tiré au sort dans la vague.
func WaveScore(wave int64, rank int64) float64 { return float64(wave*10_000_000 + rank) }
