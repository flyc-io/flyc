// flyc-load : générateur de charge qui joue le parcours réel d'un visiteur.
//
//	ticket (POST /q/<room>/ticket) → flux SSE (/events) jusqu'à « admitted » → /go → /__flyc/go sur le domaine
//	protégé (dépôt du cookie) → page finale avec le cookie.
//
// Chaque visiteur a sa propre connexion TLS (HTTP/1.1 forcé) comme un navigateur.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type stats struct {
	started, ticketOK, ticketErr, sseOpen, sseErr, admitted, goOK, goErr, finalOK, finalErr, degraded int64
	mu                                                                                                sync.Mutex
	ticketLat, admitLat, e2eLat                                                                       []float64
	errs                                                                                              map[string]int
	lastPending                                                                                       int64
}

func (s *stats) err(kind string, err error) {
	s.mu.Lock()
	k := kind + ": " + short(err)
	s.errs[k]++
	s.mu.Unlock()
}

func short(err error) string {
	m := err.Error()
	for _, cut := range []string{"dial tcp", "read tcp", "write tcp"} {
		if i := strings.Index(m, cut); i >= 0 {
			m = m[i:]
		}
	}
	if len(m) > 90 {
		m = m[:90]
	}
	return m
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := int(float64(len(c)-1) * p)
	return c[i]
}

func main() {
	waitBase := flag.String("wait", "https://wait.example.com", "URL de la salle d'attente")
	host := flag.String("host", "demo.example.com", "domaine protégé")
	room := flag.String("room", "demo.concert", "identifiant de file")
	users := flag.Int("users", 1000, "nombre de visiteurs")
	ramp := flag.Duration("ramp", 30*time.Second, "durée de montée en charge (arrivées étalées)")
	timeout := flag.Duration("timeout", 10*time.Minute, "attente maximale par visiteur")
	resolve := flag.String("resolve", "", "IP forcée pour wait et host (ex. 203.0.113.8=wait,203.0.113.7=app), vide = DNS")
	report := flag.Duration("report", 5*time.Second, "période des rapports")
	final := flag.String("path", "/billets/?src=load", "page finale demandée avec le pass")
	out := flag.String("out", "", "fichier JSON de bilan (agrégation multi-sources)")
	name := flag.String("name", "", "nom de la source dans le bilan")
	flag.Parse()

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	resolveMap := map[string]string{}
	for _, kv := range strings.Split(*resolve, ",") {
		if ip, name, ok := strings.Cut(kv, "="); ok {
			resolveMap[name] = ip
		}
	}
	waitHost := strings.TrimPrefix(strings.TrimPrefix(*waitBase, "https://"), "http://")
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, p, _ := net.SplitHostPort(addr)
		if ip, ok := resolveMap["wait"]; ok && h == waitHost {
			addr = net.JoinHostPort(ip, p)
		} else if ip, ok := resolveMap["app"]; ok && h == *host {
			addr = net.JoinHostPort(ip, p)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		tr := &http.Transport{
			DialContext:         dial,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // HTTP/1.1 : une connexion par visiteur
			ForceAttemptHTTP2:   false,
			MaxIdleConns:        4,
			MaxConnsPerHost:     4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			DisableCompression:  true,
		}
		return &http.Client{Transport: tr, Jar: jar, Timeout: 0, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	st := &stats{errs: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	t0 := time.Now()

	go func() {
		tk := time.NewTicker(*report)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				printReport(st, t0, false)
			}
		}
	}()

	gap := time.Duration(0)
	if *users > 1 {
		gap = *ramp / time.Duration(*users)
	}
	for i := 0; i < *users; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			visitor(ctx, st, newClient(), *waitBase, *host, *room, *final, *timeout)
		}(i)
		if gap > 0 {
			time.Sleep(gap)
		}
	}
	wg.Wait()
	cancel()
	printReport(st, t0, true)
	if *out != "" {
		writeJSON(*out, *name, st, t0)
	}
}

func writeJSON(path, name string, st *stats, t0 time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	rep := map[string]any{
		"source": name, "users": atomic.LoadInt64(&st.started), "tickets_ok": atomic.LoadInt64(&st.ticketOK), "tickets_err": atomic.LoadInt64(&st.ticketErr),
		"admitted": atomic.LoadInt64(&st.admitted), "final_ok": atomic.LoadInt64(&st.finalOK), "final_err": atomic.LoadInt64(&st.finalErr), "go_err": atomic.LoadInt64(&st.goErr),
		"degraded": atomic.LoadInt64(&st.degraded), "duration_s": time.Since(t0).Seconds(),
		"ticket_ms":   map[string]float64{"p50": pct(st.ticketLat, .5), "p95": pct(st.ticketLat, .95), "p99": pct(st.ticketLat, .99), "max": pct(st.ticketLat, 1)},
		"admission_s": map[string]float64{"p50": pct(st.admitLat, .5), "p95": pct(st.admitLat, .95), "max": pct(st.admitLat, 1)},
		"e2e_s":       map[string]float64{"p50": pct(st.e2eLat, .5), "p95": pct(st.e2eLat, .95), "max": pct(st.e2eLat, 1)},
		"errors":      st.errs,
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	_ = os.WriteFile(path, b, 0o644)
}

func visitor(ctx context.Context, st *stats, c *http.Client, waitBase, host, room, final string, timeout time.Duration) {
	atomic.AddInt64(&st.started, 1)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := waitBase + "/q/" + url.PathEscape(room)
	start := time.Now()

	// 1. ticket
	body, _ := json.Marshal(map[string]string{"h": host, "r": base64.StdEncoding.EncodeToString([]byte(final))})
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/ticket", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		atomic.AddInt64(&st.ticketErr, 1)
		st.err("ticket", err)
		return
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		atomic.AddInt64(&st.ticketErr, 1)
		st.err("ticket", fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(data))))
		return
	}
	var t struct {
		Ticket string `json:"ticket"`
		State  string `json:"state"`
		Go     string `json:"go"`
	}
	_ = json.Unmarshal(data, &t)
	st.mu.Lock()
	st.ticketLat = append(st.ticketLat, time.Since(start).Seconds()*1000)
	st.mu.Unlock()
	atomic.AddInt64(&st.ticketOK, 1)

	goPath := t.Go
	if t.State != "admitted" {
		// 2. SSE jusqu'à l'admission (repli polling si le LB est en mode dégradé)
		goPath = waitAdmission(ctx, st, c, base, t.Ticket)
		if goPath == "" {
			return
		}
	}
	st.mu.Lock()
	st.admitLat = append(st.admitLat, time.Since(start).Seconds())
	st.mu.Unlock()
	atomic.AddInt64(&st.admitted, 1)

	// 3. /go → Location vers https://host/__flyc/go?p=…
	req, _ = http.NewRequestWithContext(ctx, "GET", waitBase+goPath, nil)
	resp, err = c.Do(req)
	if err != nil {
		atomic.AddInt64(&st.goErr, 1)
		st.err("go", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != 302 || loc == "" {
		atomic.AddInt64(&st.goErr, 1)
		st.err("go", fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	// 4. dépôt du cookie sur le domaine protégé
	req, _ = http.NewRequestWithContext(ctx, "GET", loc, nil)
	resp, err = c.Do(req)
	if err != nil {
		atomic.AddInt64(&st.goErr, 1)
		st.err("__flyc/go", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 302 {
		atomic.AddInt64(&st.goErr, 1)
		st.err("__flyc/go", fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	atomic.AddInt64(&st.goOK, 1)
	// 5. page finale avec le cookie (le jar l'a enregistré)
	req, _ = http.NewRequestWithContext(ctx, "GET", "https://"+host+final, nil)
	resp, err = c.Do(req)
	if err != nil {
		atomic.AddInt64(&st.finalErr, 1)
		st.err("final", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		atomic.AddInt64(&st.finalErr, 1)
		st.err("final", fmt.Errorf("HTTP %d (redirigé vers %s)", resp.StatusCode, resp.Header.Get("Location")))
		return
	}
	atomic.AddInt64(&st.finalOK, 1)
	st.mu.Lock()
	st.e2eLat = append(st.e2eLat, time.Since(start).Seconds())
	st.mu.Unlock()
}

// waitAdmission suit le flux SSE ; renvoie le chemin /go à l'admission, "" en cas d'échec.
func waitAdmission(ctx context.Context, st *stats, c *http.Client, base, ticket string) string {
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/events?t="+ticket, nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := c.Do(req)
		if err != nil {
			atomic.AddInt64(&st.sseErr, 1)
			st.err("sse", err)
			return ""
		}
		if resp.StatusCode == 503 { // mode dégradé : polling
			resp.Body.Close()
			atomic.AddInt64(&st.degraded, 1)
			return pollAdmission(ctx, st, c, base, ticket)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			atomic.AddInt64(&st.sseErr, 1)
			st.err("sse", fmt.Errorf("HTTP %d", resp.StatusCode))
			return ""
		}
		atomic.AddInt64(&st.sseOpen, 1)
		goPath, retry := readSSE(ctx, st, resp.Body)
		resp.Body.Close()
		atomic.AddInt64(&st.sseOpen, -1)
		if goPath != "" {
			return goPath
		}
		if !retry {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Duration(2+rand.Intn(4)) * time.Second):
		}
	}
}

func readSSE(ctx context.Context, st *stats, body io.Reader) (goPath string, retry bool) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "admitted":
				var a struct {
					Go string `json:"go"`
				}
				_ = json.Unmarshal([]byte(data), &a)
				return a.Go, false
			case "status":
				var s struct {
					Pending int64 `json:"pending"`
				}
				_ = json.Unmarshal([]byte(data), &s)
				atomic.StoreInt64(&st.lastPending, s.Pending)
			case "closed":
				st.err("sse", fmt.Errorf("file fermée"))
				return "", false
			}
		}
	}
	if ctx.Err() != nil {
		st.err("sse", fmt.Errorf("délai d'attente dépassé"))
		return "", false
	}
	return "", true // connexion coupée : on rouvre
}

func pollAdmission(ctx context.Context, st *stats, c *http.Client, base, ticket string) string {
	for {
		select {
		case <-ctx.Done():
			st.err("poll", fmt.Errorf("délai d'attente dépassé"))
			return ""
		case <-time.After(time.Duration(10+rand.Intn(10)) * time.Second):
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/status?t="+ticket, nil)
		resp, err := c.Do(req)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var s struct {
			State string `json:"state"`
			Go    string `json:"go"`
		}
		_ = json.Unmarshal(data, &s)
		if s.State == "admitted" {
			return s.Go
		}
	}
}

func printReport(st *stats, t0 time.Time, final bool) {
	st.mu.Lock()
	tl, al, el := append([]float64(nil), st.ticketLat...), append([]float64(nil), st.admitLat...), append([]float64(nil), st.e2eLat...)
	errs := map[string]int{}
	for k, v := range st.errs {
		errs[k] = v
	}
	st.mu.Unlock()
	fmt.Printf("[%6.0fs] démarrés=%d tickets=%d(err %d) sse_ouverts=%d admis=%d go=%d(err %d) final=%d(err %d) dégradés=%d pending≈%d | ticket p50=%.0fms p95=%.0fms | admission p50=%.1fs p95=%.1fs\n",
		time.Since(t0).Seconds(), atomic.LoadInt64(&st.started), atomic.LoadInt64(&st.ticketOK), atomic.LoadInt64(&st.ticketErr), atomic.LoadInt64(&st.sseOpen),
		atomic.LoadInt64(&st.admitted), atomic.LoadInt64(&st.goOK), atomic.LoadInt64(&st.goErr), atomic.LoadInt64(&st.finalOK), atomic.LoadInt64(&st.finalErr), atomic.LoadInt64(&st.degraded),
		atomic.LoadInt64(&st.lastPending), pct(tl, .5), pct(tl, .95), pct(al, .5), pct(al, .95))
	if final {
		fmt.Printf("\n=== bilan ===\nvisiteurs        : %d\nparcours complets: %d (%.1f%%)\nticket   p50/p95/max : %.0f / %.0f / %.0f ms\nadmission p50/p95/max: %.1f / %.1f / %.1f s\nbout en bout p95     : %.1f s\ndurée totale         : %.0f s\n",
			atomic.LoadInt64(&st.started), atomic.LoadInt64(&st.finalOK), 100*float64(atomic.LoadInt64(&st.finalOK))/float64(max64(atomic.LoadInt64(&st.started), 1)),
			pct(tl, .5), pct(tl, .95), pct(tl, 1), pct(al, .5), pct(al, .95), pct(al, 1), pct(el, .95), time.Since(t0).Seconds())
		if len(errs) > 0 {
			fmt.Println("erreurs :")
			keys := make([]string, 0, len(errs))
			for k := range errs {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return errs[keys[i]] > errs[keys[j]] })
			for _, k := range keys {
				fmt.Printf("  %6d  %s\n", errs[k], k)
			}
		}
	}
	os.Stdout.Sync()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
