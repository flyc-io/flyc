package api

import (
	"encoding/base64"
	"testing"

	"github.com/flyc-io/flyc/queue/internal/room"
)

func TestNormalizeReturn(t *testing.T) {
	b := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := map[string]string{
		"":                                 "/",
		b("/billets/?x=1"):                 "/billets/?x=1",
		b("https://demo.example.com/billets/"): "/billets/",
		b("https://DEMO.example.com/a?b=c"):    "/a?b=c",
		b("https://evil.example/"):         "/",
		b("//evil.example/"):               "/",
		"/direct":                          "/direct",
		b("javascript:alert(1)"):           "/",
	}
	for in, want := range cases {
		if got := normalizeReturn(in, "demo.example.com"); got != want {
			t.Errorf("normalizeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTicketCookie(t *testing.T) {
	a := &API{opt: Options{CookieSecret: []byte("secret")}}
	id, _ := room.Parse("demo.concert")
	v := a.signTicket(id, "t_abc")
	if tid, ok := a.verifyTicket(id, v); !ok || tid != "t_abc" {
		t.Fatal("cookie valide refusé")
	}
	other, _ := room.Parse("demo.autre")
	if _, ok := a.verifyTicket(other, v); ok {
		t.Fatal("cookie d'une autre file accepté")
	}
	if _, ok := a.verifyTicket(id, "t_abc.deadbeef"); ok {
		t.Fatal("signature falsifiée acceptée")
	}
}
