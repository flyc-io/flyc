// Package keys : récupération de la clé de signature des pass auprès de flyc-control.
package keys

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Client garde la clé active et la rafraîchit périodiquement (rotation).
type Client struct {
	controlURL string
	token      string
	log        *slog.Logger
	mu         sync.RWMutex
	kid        string
	privPEM    string
	pubPEM     string
}

func New(controlURL, token string, log *slog.Logger) *Client {
	return &Client{controlURL: controlURL, token: token, log: log}
}

// Current renvoie kid et clé privée PEM ; vide tant que rien n'a été chargé.
func (c *Client) Current() (kid, privPEM string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.kid, c.privPEM
}

// Ready : une clé est chargée.
func (c *Client) Ready() bool { k, _ := c.Current(); return k != "" }

// Run charge la clé puis la rafraîchit toutes les 60 s (rotation), jusqu'à annulation du contexte.
func (c *Client) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		if err := c.fetch(ctx); err != nil {
			c.log.Warn("clé de signature indisponible", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 2 * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
}

func (c *Client) fetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.controlURL+"/internal/signing-key", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("flyc-control a répondu %d", resp.StatusCode)
	}
	var body struct {
		KID        string `json:"kid"`
		PrivatePEM string `json:"private_pem"`
		PublicPEM  string `json:"public_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if body.KID == "" || body.PrivatePEM == "" {
		return fmt.Errorf("réponse de flyc-control incomplète")
	}
	c.mu.Lock()
	changed := c.kid != body.KID
	c.kid, c.privPEM, c.pubPEM = body.KID, body.PrivatePEM, body.PublicPEM
	c.mu.Unlock()
	if changed {
		c.log.Info("clé de signature chargée", "kid", body.KID)
	}
	return nil
}
