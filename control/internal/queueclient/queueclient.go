// Package queueclient : pousse la configuration des files vers flyc-queue (API interne).
package queueclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Token string
	HTTP  *http.Client

	// La liste des hôtes de file vient de la base et change quand un nœud est ajouté ou retiré :
	// elle est relue à chaud par le réconciliateur pendant que d'autres goroutines l'utilisent.
	mu    sync.RWMutex
	hosts []string // http://ip:8080
}

// SetHosts remplace la liste des hôtes de file.
func (c *Client) SetHosts(hosts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hosts = hosts
}

// Hosts : copie de la liste courante.
func (c *Client) Hosts() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.hosts...)
}

// ParseHosts lit "name=ip:port,name=ip:port".
func ParseHosts(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, addr, ok := strings.Cut(part, "="); ok {
			part = addr
		}
		if !strings.HasPrefix(part, "http") {
			part = "http://" + part
		}
		out = append(out, part)
	}
	return out
}

func New(hosts []string, token string) *Client {
	return &Client{hosts: hosts, Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// RoomConfig tel qu'attendu par PUT /internal/rooms/{id}.
type RoomConfig struct {
	State     string            `json:"state"`
	Rate      float64           `json:"rate"`
	MaxActive int64             `json:"max_active"`
	WaveSec   int64             `json:"wave_s"`
	PassTTLS  int64             `json:"pass_ttl_s"`
	Domains   []string          `json:"domains"`
	OpensAt   int64             `json:"opens_at"`
	Title     string            `json:"title"`
	Theme     map[string]string `json:"theme,omitempty"`
}

// PutRoom essaie chaque hôte queue jusqu'au premier succès (Redis est partagé).
func (c *Client) PutRoom(ctx context.Context, id string, cfg RoomConfig) error {
	body, _ := json.Marshal(cfg)
	var last error
	for _, h := range c.Hosts() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, h+"/internal/rooms/"+id, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
		last = fmt.Errorf("%s: %d %s", h, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if last == nil {
		last = fmt.Errorf("aucun hôte queue configuré")
	}
	return last
}

// Get relaie GET /internal/rooms/{id} (stats) ou POST .../flush.
func (c *Client) Call(ctx context.Context, method, path string) (int, []byte, error) {
	var last error
	for _, h := range c.Hosts() {
		req, _ := http.NewRequestWithContext(ctx, method, h+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, data, nil
	}
	return 0, nil, last
}

// Healthy : au moins un hôte répond.
func (c *Client) Healthy(ctx context.Context, host string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, host+"/healthz", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz %d", resp.StatusCode)
	}
	return nil
}
