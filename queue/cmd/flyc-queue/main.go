// flyc-queue : service de file d'attente (tickets, vagues, admission, SSE, émission des pass).
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/flyc-io/flyc/internal/httpx"
	"github.com/flyc-io/flyc/internal/version"
	"github.com/flyc-io/flyc/queue/internal/admitter"
	"github.com/flyc-io/flyc/queue/internal/api"
	"github.com/flyc-io/flyc/queue/internal/hub"
	"github.com/flyc-io/flyc/queue/internal/keys"
	"github.com/flyc-io/flyc/queue/internal/store"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "interroge /healthz et sort avec 0 ou 1")
	flag.Parse()

	listen := httpx.Env("FLYC_LISTEN", ":8080")
	if *healthcheck {
		os.Exit(httpx.Healthcheck(listen))
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log = log.With("service", "flyc-queue", "version", version.Version)

	addrs := strings.Split(httpx.MustEnv("FLYC_REDIS_ADDRS", log), ",")
	internalToken := httpx.MustEnv("FLYC_INTERNAL_TOKEN", log)
	controlURL := httpx.MustEnv("FLYC_CONTROL_URL", log)

	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        addrs,
		Password:     os.Getenv("FLYC_REDIS_PASSWORD"),
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     128,
		MinIdleConns: 8,
	})
	defer rdb.Close()
	st := store.New(rdb)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pas d'attente bloquante : /healthz répond 503 tant que le cluster n'est pas prêt.
	go func() {
		for i := 1; ; i++ {
			c, cc := context.WithTimeout(ctx, 3*time.Second)
			err := redisHealthy(rdb)(c)
			cc()
			if err == nil {
				log.Info("redis cluster prêt", "nodes", len(addrs), "attempts", i)
				return
			}
			if i == 1 || i%20 == 0 {
				log.Warn("redis cluster pas encore prêt", "attempt", i, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_queue_build_info", Help: "Version de flyc-queue"}, []string{"version"})
	reg.MustRegister(buildInfo)
	buildInfo.WithLabelValues(version.Version).Set(1)
	redisOK := prometheus.NewGauge(prometheus.GaugeOpts{Name: "flyc_queue_redis_ok", Help: "1 si le cluster Redis répond et est en état ok"})
	reg.MustRegister(redisOK)
	go func() {
		for {
			c, cc := context.WithTimeout(ctx, 3*time.Second)
			if redisHealthy(rdb)(c) == nil {
				redisOK.Set(1)
			} else {
				redisOK.Set(0)
			}
			cc()
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()

	kc := keys.New(controlURL, internalToken, log)
	go kc.Run(ctx)

	h := hub.New(st, reg, log)
	mgr := admitter.NewManager(st, kc, admitter.NewMetrics(reg), log)
	go mgr.Run(ctx)

	cookieSecret := sha256.Sum256([]byte("flyc-ticket|" + internalToken))
	a := api.New(st, h, kc, api.Options{
		InternalToken:    internalToken,
		CookieSecret:     cookieSecret[:],
		TurnstileSecret:  os.Getenv("FLYC_TURNSTILE_SECRET"),
		TurnstileSiteKey: os.Getenv("FLYC_TURNSTILE_SITEKEY"),
		WaitHost:         os.Getenv("FLYC_WAIT_HOST"),
	}, reg, log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.HealthHandler(
		httpx.Checker{Name: "redis", Check: redisHealthy(rdb)},
		httpx.Checker{Name: "signing_key", Check: func(context.Context) error {
			if !kc.Ready() {
				return fmt.Errorf("clé de signature pas encore chargée")
			}
			return nil
		}},
	))
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]string{"service": "flyc-queue", "version": version.Version})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	a.Register(mux)

	if err := httpx.Serve(listen, mux, log); err != nil {
		log.Error("serveur arrêté en erreur", "err", err)
		os.Exit(1)
	}
}

// redisHealthy : PING + cluster_state:ok.
func redisHealthy(rdb *redis.ClusterClient) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if err := rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("ping: %w", err)
		}
		info, err := rdb.ClusterInfo(ctx).Result()
		if err != nil {
			return fmt.Errorf("cluster info: %w", err)
		}
		if !strings.Contains(info, "cluster_state:ok") {
			return fmt.Errorf("cluster_state pas ok")
		}
		return nil
	}
}
