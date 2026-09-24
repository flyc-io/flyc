// flyc-control : control plane (API, UI, compilation et push de la config edge).
// M0 : socle HTTP, Postgres avec migrations embarquées, bootstrap du premier admin,
// génération de la paire de clés de signature des pass.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/flyc-io/flyc/control/internal/acme"
	"github.com/flyc-io/flyc/control/internal/api"
	"github.com/flyc-io/flyc/control/internal/auth"
	"github.com/flyc-io/flyc/control/internal/bootstrap"
	"github.com/flyc-io/flyc/control/internal/crypt"
	"github.com/flyc-io/flyc/control/internal/db"
	"github.com/flyc-io/flyc/control/internal/inventory"
	"github.com/flyc-io/flyc/control/internal/jobs"
	"github.com/flyc-io/flyc/control/internal/queueclient"
	"github.com/flyc-io/flyc/control/internal/reconcile"
	"github.com/flyc-io/flyc/control/internal/ui"
	"github.com/flyc-io/flyc/internal/httpx"
	"github.com/flyc-io/flyc/internal/pass"
	"github.com/flyc-io/flyc/internal/version"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "interroge /healthz et sort avec 0 ou 1")
	flag.Parse()

	listen := httpx.Env("FLYC_LISTEN", ":8090")
	if *healthcheck {
		os.Exit(httpx.Healthcheck(listen))
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log = log.With("service", "flyc-control", "version", version.Version)

	dsn := httpx.MustEnv("FLYC_DATABASE_URL", log)
	masterKey := httpx.MustEnv("FLYC_MASTER_KEY", log)
	dataDir := httpx.Env("FLYC_DATA_DIR", "/data")

	pool := connectWithRetry(dsn, log)
	defer pool.Close()

	if err := db.Migrate(context.Background(), pool, log); err != nil {
		log.Error("migrations", "err", err)
		os.Exit(1)
	}

	boot, err := bootstrap.Run(context.Background(), pool, masterKey, dataDir, log)
	if err != nil {
		log.Error("bootstrap", "err", err)
		os.Exit(1)
	}

	// Sous-commandes d'exploitation : `flyc-control mint-pass --dom ... --room ... [--ttl 20m]`
	if flag.NArg() > 0 && flag.Arg(0) == "mint-pass" {
		os.Exit(mintPass(flag.Args()[1:], boot))
	}

	// Control plane : réconciliation des LB et de flyc-queue, ACME, API v1.
	mk, err := crypt.Parse(masterKey)
	if err != nil {
		log.Error("clé maître", "err", err)
		os.Exit(1)
	}
	internalToken := httpx.MustEnv("FLYC_INTERNAL_TOKEN", log)
	// L'inventaire vit en base. FLYC_INVENTORY_SEED, rendu par Ansible, ne sert qu'au tout premier
	// démarrage : il amorce les tables depuis le fichier qui a installé la plateforme. Les listes
	// de LB et d'hôtes de file sont ensuite relues à chaque passe de réconciliation, ce qui permet
	// d'ajouter un nœud sans redémarrer control.
	seed := inventory.Lire(os.Getenv("FLYC_INVENTORY_SEED_FILE"), os.Getenv("FLYC_INVENTORY_SEED"))
	if err := inventory.Apply(context.Background(), pool, seed, log); err != nil {
		log.Error("amorçage de l'inventaire", "err", err)
		os.Exit(1)
	}
	qc := queueclient.New(nil, internalToken)
	rec := reconcile.New(pool, httpx.Env("FLYC_DATAPLANE_USER", "flyc"), os.Getenv("FLYC_DATAPLANE_PASSWORD"), qc, mk.Open, log)
	rec.SetWaitHost(os.Getenv("FLYC_WAIT_HOST"))
	ac := acme.New(pool, mk, os.Getenv("FLYC_ACME_DIRECTORY"), os.Getenv("FLYC_ACME_EMAIL"), log, rec.Kick)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)
	go rec.RunAuto(ctx)
	go ac.Run(ctx)

	// Interface web : sessions, OIDC optionnel.
	publicURL := strings.TrimRight(httpx.Env("FLYC_PUBLIC_URL", "https://"+os.Getenv("FLYC_CONTROL_HOST")), "/")
	sessions := &auth.Sessions{Pool: pool, Secure: httpx.Env("FLYC_COOKIE_SECURE", "true") != "false"}
	oidc := auth.NewOIDC(ctx, os.Getenv("FLYC_OIDC_ISSUER"), os.Getenv("FLYC_OIDC_CLIENT_ID"), os.Getenv("FLYC_OIDC_CLIENT_SECRET"), publicURL+"/auth/oidc/callback", os.Getenv("FLYC_OIDC_ADMIN_EMAILS"), log)
	go func() {
		for {
			sessions.Purge(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
	v1 := api.New(pool, rec, ac, qc, sessions, oidc, mk, "Flyc", log)
	v1.SetSigning(boot)

	// Le runner exécute les playbooks à notre place : on ne fait que déposer des demandes dans le
	// répertoire partagé et suivre ce qu'il y écrit. Absent (installation par fichier), les routes
	// de tâches répondent 503 et le reste fonctionne.
	if dir := os.Getenv("FLYC_JOBS_DIR"); dir != "" {
		jm := jobs.New(pool, dir, log)
		v1.SetJobs(jm)
		go jm.Run(ctx)
		log.Info("exécuteur de tâches actif", "répertoire", dir)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_control_build_info", Help: "Version de flyc-control"}, []string{"version"})
	reg.MustRegister(buildInfo)
	buildInfo.WithLabelValues(version.Version).Set(1)
	rec.SetMetrics(reconcile.NewMetrics(reg))
	rec.Kick()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", httpx.HealthHandler(httpx.Checker{Name: "postgres", Check: pool.Ping}))
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]string{"service": "flyc-control", "version": version.Version})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /.well-known/acme-challenge/", ac.Handler())
	v1.Register(mux)
	mux.Handle("GET /", ui.Handler())

	if err := httpx.Serve(listen, mux, log); err != nil {
		log.Error("serveur arrêté en erreur", "err", err)
		os.Exit(1)
	}
}

// mintPass émet un pass signé avec la clé active. Usage d'exploitation et de test (jalon M1).
func mintPass(args []string, boot *bootstrap.Result) int {
	fs := flag.NewFlagSet("mint-pass", flag.ContinueOnError)
	dom := fs.String("dom", "", "domaine protégé (claim dom, comparé au Host par HAProxy)")
	room := fs.String("room", "", "identifiant de file (claim rm)")
	tid := fs.String("tenant", "", "tenant (claim tid)")
	sub := fs.String("sub", "", "identifiant du ticket (claim sub), aléatoire si vide")
	ttl := fs.Duration("ttl", 20*time.Minute, "durée de validité")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dom == "" || *room == "" {
		fmt.Fprintln(os.Stderr, "mint-pass : --dom et --room sont obligatoires")
		return 2
	}
	tok, err := pass.Mint(boot.PrivatePEM, boot.KID, pass.Claims{Sub: *sub, Tenant: *tid, Dom: *dom, Room: *room, TTL: *ttl})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint-pass :", err)
		return 1
	}
	fmt.Println(tok)
	return 0
}

func connectWithRetry(dsn string, log *slog.Logger) *pgxpool.Pool {
	for i := 1; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			err = pool.Ping(ctx)
		}
		cancel()
		if err == nil {
			log.Info("postgres connecté")
			return pool
		}
		log.Warn("postgres indisponible, nouvelle tentative", "attempt", i, "err", err)
		if i >= 60 {
			log.Error("postgres injoignable après 60 tentatives")
			os.Exit(1)
		}
		time.Sleep(3 * time.Second)
	}
}
