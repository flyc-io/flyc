// Package reconcile : applique l'état désiré à chaque LB (Dataplane API) et à flyc-queue.
//
// Boucle : à chaque changement de config_version (ou toutes les 60 s), Build() puis Push() par edge.
// Un LB en échec n'empêche pas les autres ; son erreur est enregistrée dans edges.last_error.
package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/flyc-io/flyc/control/internal/compile"
	"github.com/flyc-io/flyc/control/internal/dataplane"
	"github.com/flyc-io/flyc/control/internal/inventory"
	"github.com/flyc-io/flyc/control/internal/queueclient"
)

// Metrics exposées par le control plane.
type Metrics struct {
	EdgeInSync *prometheus.GaugeVec
	CertExpiry *prometheus.GaugeVec
	CertStatus *prometheus.GaugeVec
	Rooms      *prometheus.GaugeVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		EdgeInSync: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_control_edge_in_sync", Help: "1 si le LB porte l'état désiré"}, []string{"edge"}),
		CertExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_control_certificate_expiry_seconds", Help: "Expiration du certificat (unix)"}, []string{"fqdn"}),
		CertStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_control_certificate_status", Help: "1 pour le statut courant"}, []string{"fqdn", "status"}),
		Rooms:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "flyc_control_rooms", Help: "Files par état effectif"}, []string{"state"}),
	}
	reg.MustRegister(m.EdgeInSync, m.CertExpiry, m.CertStatus, m.Rooms)
	return m
}

// Edge : alias du type porté par inventory, qui est la source des LB.
type Edge = inventory.Edge

type Reconciler struct {
	m        *Metrics
	pool     *pgxpool.Pool
	emu      sync.RWMutex
	edges    []Edge
	dpUser   string
	dpPass   string
	queue    *queueclient.Client
	decrypt  func([]byte) ([]byte, error)
	log      *slog.Logger
	sslDir   string
	waitHost string
	kick     chan struct{}
	mu       sync.Mutex
	lastVer  int64
	// certificats déjà poussés par edge : fqdn → hash
	pushedCerts map[string]map[string]string
}

func New(pool *pgxpool.Pool, dpUser, dpPass string, q *queueclient.Client, decrypt func([]byte) ([]byte, error), log *slog.Logger) *Reconciler {
	return &Reconciler{pool: pool, dpUser: dpUser, dpPass: dpPass, queue: q, decrypt: decrypt, log: log,
		sslDir: "/etc/haproxy/ssl", kick: make(chan struct{}, 1), pushedCerts: map[string]map[string]string{}}
}

// SetMetrics branche les métriques Prometheus.
func (r *Reconciler) SetMetrics(m *Metrics) { r.m = m }

// SetWaitHost : le certificat de ce domaine va dans la crt-list de la salle d'attente.
func (r *Reconciler) SetWaitHost(h string) { r.waitHost = strings.ToLower(h) }

// reloadTopology relit les nœuds en base : LB à qui pousser, hôtes de file à qui parler. Appelée
// à chaque passe — c'est ce qui permet d'ajouter un LB sans redémarrer control, là où la liste
// venait d'une variable d'environnement lue une seule fois au démarrage.
func (r *Reconciler) reloadTopology(ctx context.Context) {
	var incomplet inventory.ErrIncomplete
	if err := inventory.Sync(ctx, r.pool); errors.As(err, &incomplet) {
		// Base pas encore amorcée : on garde la topologie en mémoire plutôt que d'en déduire un
		// vide, et on le dit une fois par passe.
		r.log.Warn("inventaire", "raison", incomplet.Raison)
		return
	} else if err != nil {
		r.log.Warn("synchro nodes → edges/queue_hosts", "err", err)
	}
	edges, err := inventory.Edges(ctx, r.pool)
	if err != nil {
		r.log.Warn("lecture des LB", "err", err)
		return
	}
	r.emu.Lock()
	r.edges = edges
	r.emu.Unlock()
	hosts, err := inventory.QueueHosts(ctx, r.pool)
	if err != nil {
		r.log.Warn("lecture des hôtes de file", "err", err)
		return
	}
	r.queue.SetHosts(hosts)
}

// Edges : copie de la liste courante des LB.
func (r *Reconciler) Edges() []Edge {
	r.emu.RLock()
	defer r.emu.RUnlock()
	return append([]Edge(nil), r.edges...)
}

// Kick demande une réconciliation immédiate.
func (r *Reconciler) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// EdgeNames : noms des LB en service. Sert à distinguer un LB encore déployé d'un LB sorti de
// l'inventaire, dont la ligne en base peut être supprimée.
func (r *Reconciler) EdgeNames() []string {
	edges := r.Edges()
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.Name)
	}
	return out
}

func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	r.Once(ctx, true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
			r.Once(ctx, true)
		case <-t.C:
			r.Once(ctx, true)
		case <-poll.C:
			var v int64
			if err := r.pool.QueryRow(ctx, `select version from config_version where id=1`).Scan(&v); err == nil && v != r.lastVer {
				r.Once(ctx, true)
			}
		}
	}
}

// Once : une réconciliation complète. force=true pousse même si le hash est inchangé.
func (r *Reconciler) Once(ctx context.Context, force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reloadTopology(ctx)
	var ver int64
	_ = r.pool.QueryRow(ctx, `select version from config_version where id=1`).Scan(&ver)
	d, err := compile.Build(ctx, r.pool, r.decrypt)
	if err != nil {
		r.log.Error("compilation", "err", err)
		return
	}
	// flyc-queue : config des files
	for id, cfg := range d.Queue {
		if err := r.queue.PutRoom(ctx, id, cfg); err != nil {
			r.log.Warn("push file vers flyc-queue", "room", id, "err", err)
			_, _ = r.pool.Exec(ctx, `update queue_hosts set last_error=$1`, err.Error())
		} else {
			_, _ = r.pool.Exec(ctx, `update queue_hosts set last_seen_at=now(), last_error=null`)
		}
	}
	// edges
	for _, e := range r.Edges() {
		if err := r.pushEdge(ctx, e, d); err != nil {
			r.log.Warn("push edge", "edge", e.Name, "err", err)
			_, _ = r.pool.Exec(ctx, `update edges set last_push_at=now(), last_push_status='error', last_error=$2, desired_hash=$3 where name=$1`, e.Name, err.Error(), d.Hash)
		} else {
			_, _ = r.pool.Exec(ctx, `update edges set last_push_at=now(), last_push_status='ok', last_error=null, config_hash=$2, desired_hash=$2, last_seen_at=now() where name=$1`, e.Name, d.Hash)
		}
	}
	r.lastVer = ver
	r.updateMetrics(ctx, d)
}

func (r *Reconciler) updateMetrics(ctx context.Context, d *compile.Desired) {
	if r.m == nil {
		return
	}
	rows, err := r.pool.Query(ctx, `select name, coalesce(config_hash,'') = coalesce(desired_hash,'') and config_hash is not null from edges`)
	if err == nil {
		for rows.Next() {
			var name string
			var ok bool
			if rows.Scan(&name, &ok) == nil {
				v := 0.0
				if ok {
					v = 1
				}
				r.m.EdgeInSync.WithLabelValues(name).Set(v)
			}
		}
		rows.Close()
	}
	r.m.CertStatus.Reset()
	crows, err := r.pool.Query(ctx, `select fqdn, status, not_after from certificates`)
	if err == nil {
		for crows.Next() {
			var fqdn, status string
			var notAfter *time.Time
			if crows.Scan(&fqdn, &status, &notAfter) == nil {
				r.m.CertStatus.WithLabelValues(fqdn, status).Set(1)
				if notAfter != nil {
					r.m.CertExpiry.WithLabelValues(fqdn).Set(float64(notAfter.Unix()))
				}
			}
		}
		crows.Close()
	}
	counts := map[string]float64{"open": 0, "queue": 0, "closed": 0}
	for _, e := range d.States {
		counts[e.Value]++
	}
	for st, n := range counts {
		r.m.Rooms.WithLabelValues(st).Set(n)
	}
}

func (r *Reconciler) pushEdge(ctx context.Context, e Edge, d *compile.Desired) error {
	c := dataplane.New(e.URL, r.dpUser, r.dpPass)
	if _, err := c.Info(ctx); err != nil {
		return fmt.Errorf("injoignable: %w", err)
	}
	// 1. Certificats : fichiers sur disque, puis crt-lists complètes (une ligne par domaine, SNI filtré).
	//    Un changement de crt-list déclenche un reload par la Dataplane API. Chargement à chaud prévu en M6.
	pushed := r.pushedCerts[e.Name]
	if pushed == nil {
		pushed = map[string]string{}
		r.pushedCerts[e.Name] = pushed
	}
	// Le certificat de démarrage porte les noms de la plateforme en SAN : à égalité de correspondance SNI,
	// HAProxy prend le premier de la liste, donc les certificats émis passent avant lui.
	var appList, waitList []string
	for _, cert := range d.Certs {
		name := dataplane.StorageName(cert.FQDN)
		path := r.sslDir + "/" + name
		if pushed[cert.FQDN] != cert.Hash {
			if _, err := c.UpsertCert(ctx, name, cert.PEM); err != nil {
				return fmt.Errorf("cert %s: %w", cert.FQDN, err)
			}
			pushed[cert.FQDN] = cert.Hash
			r.log.Info("certificat déposé", "edge", e.Name, "fqdn", cert.FQDN)
		}
		line := path + " " + cert.FQDN
		if cert.FQDN == r.waitHost {
			waitList = append(waitList, line)
		} else {
			appList = append(appList, line)
		}
	}
	appList = append(appList, r.sslDir+"/bootstrap.pem")
	waitList = append(waitList, r.sslDir+"/bootstrap.pem")
	for _, l := range []struct{ name, content string }{{"app.crt-list", strings.Join(appList, "\n") + "\n"}, {"wait.crt-list", strings.Join(waitList, "\n") + "\n"}} {
		changed, err := c.CrtListSync(ctx, l.name, l.content)
		if err != nil {
			return fmt.Errorf("%s: %w", l.name, err)
		}
		if changed {
			r.log.Info("crt-list mise à jour (reload)", "edge", e.Name, "list", l.name)
		}
	}
	// 1c. Clés publiques de vérification des pass (courante + précédente), dans le stockage général ;
	//     un changement demande un reload (jwt_verify charge les fichiers au démarrage).
	keysChanged := false
	for _, k := range []struct{ name, pem string }{{"pass.pub", d.PassPub}, {"pass-prev.pub", d.PassPrevPub}} {
		if k.pem == "" {
			continue
		}
		changed, err := c.GeneralSync(ctx, k.name, k.pem)
		if err != nil {
			return fmt.Errorf("%s: %w", k.name, err)
		}
		if changed {
			keysChanged = true
		}
	}
	if keysChanged {
		if err := c.Reload(ctx); err != nil {
			return fmt.Errorf("reload après rotation de clé: %w", err)
		}
		r.log.Info("clés de vérification mises à jour (reload)", "edge", e.Name)
	}
	// 2. Backends : diff avec l'existant, dans une transaction (reload seulement si quelque chose change)
	existing, err := c.Backends(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, b := range existing {
		if strings.HasPrefix(b.Name, "bk_") && strings.Contains(b.Name, "__") {
			have[b.Name] = true
		}
	}
	want := map[string]compile.BackendSpec{}
	for _, b := range d.Backends {
		want[b.Name] = b
	}
	changed := false
	tx, err := c.BeginTx(ctx)
	if err != nil {
		return err
	}
	for name := range have {
		if _, ok := want[name]; !ok {
			if err := c.DeleteBackend(ctx, tx.ID, name); err != nil {
				c.AbortTx(ctx, tx.ID)
				return err
			}
			changed = true
		}
	}
	for name, spec := range want {
		bk := dataplane.Backend{Name: name, Mode: "http", Balance: &dataplane.Balance{Algorithm: spec.Balance},
			AdvCheck: "httpchk", HTTPChkParams: &dataplane.HTTPChkParams{Method: "GET", URI: spec.CheckPath},
			DefaultServer: &dataplane.DefaultServer{Check: "enabled", Inter: 5000, Rise: 2, Fall: 3}}
		if !have[name] {
			if err := c.CreateBackend(ctx, tx.ID, bk); err != nil {
				c.AbortTx(ctx, tx.ID)
				return err
			}
			for _, s := range spec.Servers {
				if err := c.CreateServer(ctx, tx.ID, name, s); err != nil {
					c.AbortTx(ctx, tx.ID)
					return err
				}
			}
			changed = true
			continue
		}
		cur, err := c.Servers(ctx, name)
		if err != nil {
			c.AbortTx(ctx, tx.ID)
			return err
		}
		curMap := map[string]dataplane.Server{}
		for _, s := range cur {
			curMap[s.Name] = s
		}
		for _, s := range spec.Servers {
			if old, ok := curMap[s.Name]; !ok {
				if err := c.CreateServer(ctx, tx.ID, name, s); err != nil {
					c.AbortTx(ctx, tx.ID)
					return err
				}
				changed = true
			} else if old.Address != s.Address || old.Port != s.Port || (s.Weight != nil && (old.Weight == nil || *old.Weight != *s.Weight)) {
				if err := c.ReplaceServer(ctx, tx.ID, name, s); err != nil {
					c.AbortTx(ctx, tx.ID)
					return err
				}
				changed = true
			}
			delete(curMap, s.Name)
		}
		for sname := range curMap {
			if err := c.DeleteServer(ctx, tx.ID, name, sname); err != nil {
				c.AbortTx(ctx, tx.ID)
				return err
			}
			changed = true
		}
	}
	if changed {
		if err := c.CommitTx(ctx, tx.ID); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		r.log.Info("backends mis à jour (reload)", "edge", e.Name)
	} else {
		c.AbortTx(ctx, tx.ID)
	}
	// 3. Maps : remplacement complet à chaud (aucun reload)
	if err := c.SyncMap(ctx, "backends.map", d.Backend); err != nil {
		return fmt.Errorf("backends.map: %w", err)
	}
	if err := c.SyncMap(ctx, "rooms.map", d.Rooms); err != nil {
		return fmt.Errorf("rooms.map: %w", err)
	}
	if err := c.SyncMap(ctx, "states.map", d.States); err != nil {
		return fmt.Errorf("states.map: %w", err)
	}
	return nil
}

// HashPEM : utilitaire pour l'API (état de déploiement des certificats).
func HashPEM(pem []byte) string {
	h := sha256.Sum256(pem)
	return hex.EncodeToString(h[:])
}

// RunAuto : toutes les 5 s, pour chaque file en mode auto, mesure les sessions du backend sur tous les LB
// et bascule l'état effectif (open ↔ queue) selon les seuils. Hystérésis : on entre en file au-dessus de
// auto_on, on en sort en dessous de auto_off.
func (r *Reconciler) RunAuto(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rows, err := r.pool.Query(ctx, `select r.id, t.slug, r.slug, r.effective_state, r.auto_on, r.auto_off, bt.slug, b.name
			from rooms r join tenants t on t.id=r.tenant_id
			left join backends b on b.id=r.backend_id left join tenants bt on bt.id=b.tenant_id
			where r.state='auto' and b.id is not null`)
		if err != nil {
			continue
		}
		type row struct {
			id, tslug, rslug, eff string
			on, off               int64
			bk                    string
		}
		var list []row
		for rows.Next() {
			var x row
			var btslug, bname string
			if rows.Scan(&x.id, &x.tslug, &x.rslug, &x.eff, &x.on, &x.off, &btslug, &bname) == nil {
				x.bk = compile.BackendName(btslug, bname)
				list = append(list, x)
			}
		}
		rows.Close()
		for _, x := range list {
			var total int64
			okEdges := 0
			for _, e := range r.Edges() {
				n, err := dataplane.New(e.URL, r.dpUser, r.dpPass).BackendSessions(ctx, x.bk)
				if err != nil {
					continue
				}
				total += n
				okEdges++
			}
			if okEdges == 0 {
				continue
			}
			next := x.eff
			if x.eff != "queue" && x.on > 0 && total > x.on {
				next = "queue"
			} else if x.eff == "queue" && total < x.off {
				next = "open"
			}
			if next != x.eff {
				if _, err := r.pool.Exec(ctx, `update rooms set effective_state=$2, updated_at=now() where id=$1`, x.id, next); err == nil {
					r.log.Info("mode auto : bascule", "room", x.tslug+"."+x.rslug, "sessions", total, "from", x.eff, "to", next)
					_, _ = r.pool.Exec(ctx, `insert into audit_log(tenant_id, action, diff) select tenant_id, 'room.auto', $2 from rooms where id=$1`, x.id, fmt.Sprintf(`{"sessions":%d,"state":"%s"}`, total, next))
					r.Kick()
				}
			}
		}
	}
}
