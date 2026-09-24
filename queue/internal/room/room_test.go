package room

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	id, err := Parse("demo.concert-2026")
	if err != nil || id.Tenant != "demo" || id.Room != "concert-2026" {
		t.Fatalf("parse: %v %+v", err, id)
	}
	if id.Key("pending") != "{r:demo:concert-2026}:pending" {
		t.Fatalf("clé inattendue : %s", id.Key("pending"))
	}
	for _, bad := range []string{"demo", "Demo.x", "a.b.c", ".x", "a.", "a b.c"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q devrait être refusé", bad)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	c := Config{State: Queue, Rate: 2.5, MaxActive: 100, WaveSec: 20, PassTTL: 10 * time.Minute, Domains: []string{"Demo.Example.com"}, Title: "Concert"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	back, ok := FromHash(func() map[string]string {
		m := map[string]string{}
		for k, v := range c.ToHash() {
			m[k] = v.(string)
		}
		return m
	}())
	if !ok || back.Rate != 2.5 || back.WaveSec != 20 || back.PassTTL != 10*time.Minute || !back.AllowsDomain("demo.example.com") || back.Title != "Concert" {
		t.Fatalf("round trip: %+v", back)
	}
	if back.AllowsDomain("autre.fr") {
		t.Fatal("domaine non listé accepté")
	}
}

func TestWaveScoreOrder(t *testing.T) {
	// Toute la vague n passe avant la vague n+1, quel que soit le rang.
	if WaveScore(10, 9_999_999) >= WaveScore(11, 0) {
		t.Fatal("les vagues doivent rester ordonnées")
	}
}
