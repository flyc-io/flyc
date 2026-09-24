// Package httpx : helpers HTTP communs à flyc-queue et flyc-control.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// JSON écrit une réponse JSON avec le code donné.
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Error écrit une erreur JSON {"error": code, "message": msg}.
func Error(w http.ResponseWriter, status int, code, msg string) {
	JSON(w, status, map[string]string{"error": code, "message": msg})
}

// Checker est une vérification de santé nommée.
type Checker struct {
	Name  string
	Check func(ctx context.Context) error
}

// HealthHandler renvoie 200 si toutes les vérifications passent, 503 sinon,
// avec le détail par composant.
func HealthHandler(checks ...Checker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status := http.StatusOK
		out := map[string]string{}
		for _, c := range checks {
			if err := c.Check(ctx); err != nil {
				out[c.Name] = err.Error()
				status = http.StatusServiceUnavailable
			} else {
				out[c.Name] = "ok"
			}
		}
		JSON(w, status, map[string]any{"status": map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK], "checks": out})
	}
}

// Serve démarre le serveur et l'arrête proprement sur SIGINT/SIGTERM.
func Serve(addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	log.Info("listening", "addr", addr)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// Env lit une variable d'environnement avec valeur par défaut.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// MustEnv lit une variable obligatoire ou arrête le programme.
func MustEnv(key string, log *slog.Logger) string {
	v := os.Getenv(key)
	if v == "" {
		log.Error("variable d'environnement manquante", "key", key)
		os.Exit(2)
	}
	return v
}

// Healthcheck interroge /healthz en local (utilisé par le HEALTHCHECK Docker).
func Healthcheck(addr string) int {
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
