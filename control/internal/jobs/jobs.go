// Package jobs pilote les opérations d'infrastructure que flyc-control ne peut pas exécuter
// lui-même.
//
// Le conteneur de control est une image minimale sans Python ni SSH, et il ne doit pas voir la
// clé privée de déploiement. Un second conteneur, le runner, fait le travail. Les deux
// n'échangent que par des fichiers dans un répertoire partagé : pas de service réseau de plus,
// donc pas d'authentification de plus, et surtout un protocole qui survit au redémarrage de
// control — ce qui arrive à chaque déploiement qui le vise.
//
//	<id>.request.json   déposé par control
//	<id>.claim          créé par le runner quand il prend la tâche
//	<id>.log            écrit au fil de l'eau, suivi en direct par l'interface
//	<id>.result.json    écrit à la fin
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBusy : une tâche est déjà en cours. Ces opérations touchent la même plateforme, les laisser
// se chevaucher n'aurait pas de sens ; la base porte la contrainte (index partiel unique).
var ErrBusy = errors.New("une tâche est déjà en cours")

// Request décrit ce que le runner doit exécuter. Le format est partagé avec runner/run.py.
type Request struct {
	Kind      string         `json:"kind"`                 // playbook | script
	Playbook  string         `json:"playbook,omitempty"`   // chemin dans le dépôt embarqué
	Argv      []string       `json:"argv,omitempty"`       // pour kind=script
	Limit     string         `json:"limit,omitempty"`      // --limit
	Tags      string         `json:"tags,omitempty"`       // --tags
	Check     bool           `json:"check,omitempty"`      // --check --diff
	ExtraVars map[string]any `json:"extra_vars,omitempty"` // -e
	Inventory string         `json:"inventory,omitempty"`  // inventaire complet, en YAML
}

type Job struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Ce qui a été demandé, sans l'inventaire : il pèse quelques kilo-octets et n'apprend rien
	// dans une liste. Il reste dans la demande déposée pour le runner.
	Request Request `json:"request"`
}

// Done : la tâche ne bougera plus.
func (j Job) Done() bool {
	return j.Status == "succeeded" || j.Status == "failed" || j.Status == "interrupted"
}

type Manager struct {
	pool *pgxpool.Pool
	dir  string
	log  *slog.Logger
}

func New(pool *pgxpool.Pool, dir string, log *slog.Logger) *Manager {
	return &Manager{pool: pool, dir: dir, log: log.With("composant", "jobs")}
}

func (m *Manager) path(id, suffix string) string { return filepath.Join(m.dir, id+suffix) }

// LogPath : emplacement du journal d'une tâche.
func (m *Manager) LogPath(id string) string { return m.path(id, ".log") }

// Submit enregistre la tâche puis dépose la demande. L'ordre compte : si le dépôt échoue, la
// ligne est retirée, sinon une tâche fantôme bloquerait la file pour toujours.
func (m *Manager) Submit(ctx context.Context, kind string, req Request, createdBy string) (*Job, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var id string
	err = m.pool.QueryRow(ctx,
		`insert into jobs(kind, request, created_by) values($1,$2,$3) returning id::text`,
		kind, raw, createdBy).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return nil, ErrBusy
		}
		return nil, err
	}
	if err := m.write(id, req); err != nil {
		_, _ = m.pool.Exec(ctx, `delete from jobs where id=$1`, id)
		return nil, fmt.Errorf("dépôt de la demande : %w", err)
	}
	m.log.Info("tâche déposée", "id", id, "kind", kind, "par", createdBy)
	return m.Get(ctx, id)
}

// write dépose la demande de façon atomique : le runner ne doit jamais lire un fichier à moitié
// écrit.
func (m *Manager) write(id string, req Request) error {
	if err := os.MkdirAll(m.dir, 0o770); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	tmp := m.path(id, ".request.tmp")
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, m.path(id, ".request.json"))
}

func (m *Manager) Get(ctx context.Context, id string) (*Job, error) {
	rows, _ := m.pool.Query(ctx, selectJobs+` where id=$1`, id)
	list, err := scan(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, pgx.ErrNoRows
	}
	return &list[0], nil
}

func (m *Manager) List(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, _ := m.pool.Query(ctx, selectJobs+` order by created_at desc limit $1`, limit)
	return scan(rows)
}

const selectJobs = `select id::text, kind, status, exit_code, coalesce(detail,''),
       coalesce(created_by,''), created_at, started_at, finished_at, request from jobs`

func scan(rows pgx.Rows) ([]Job, error) {
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var j Job
		var brut []byte
		if err := rows.Scan(&j.ID, &j.Kind, &j.Status, &j.ExitCode, &j.Detail,
			&j.CreatedBy, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &brut); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(brut, &j.Request); err == nil {
			j.Request.Inventory = ""
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Run suit l'avancement des tâches : le runner ne parle pas à la base, c'est control qui lit ses
// fichiers et met l'état à jour. Une seconde de battement suffit, ces opérations durent des
// minutes.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		m.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) refresh(ctx context.Context) {
	rows, _ := m.pool.Query(ctx, selectJobs+` where status in ('pending','running')`)
	list, err := scan(rows)
	if err != nil {
		return
	}
	for _, j := range list {
		if j.Status == "pending" {
			if _, err := os.Stat(m.path(j.ID, ".claim")); err == nil {
				_, _ = m.pool.Exec(ctx,
					`update jobs set status='running', started_at=coalesce(started_at, now()) where id=$1`, j.ID)
			}
		}
		body, err := os.ReadFile(m.path(j.ID, ".result.json"))
		if err != nil {
			continue
		}
		var res struct {
			RC     int    `json:"rc"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			continue
		}
		status := "succeeded"
		switch {
		case res.RC == 75: // le runner s'est arrêté en cours de route
			status = "interrupted"
		case res.RC != 0:
			status = "failed"
		}
		if _, err := m.pool.Exec(ctx, `update jobs set status=$2, exit_code=$3, detail=nullif($4,''),
		        started_at=coalesce(started_at, now()), finished_at=now() where id=$1`,
			j.ID, status, res.RC, res.Detail); err != nil {
			m.log.Warn("mise à jour de la tâche", "id", j.ID, "err", err)
			continue
		}
		m.log.Info("tâche terminée", "id", j.ID, "statut", status, "code", res.RC)
		m.cleanup(j.ID)
	}
}

// cleanup retire les fichiers de travail et garde le journal : c'est lui qu'on relira.
func (m *Manager) cleanup(id string) {
	for _, suffix := range []string{".request.json", ".claim", ".result.json"} {
		_ = os.Remove(m.path(id, suffix))
	}
}

// Tail écrit le journal d'une tâche au fil de l'eau. Il suit le fichier plutôt qu'un flux en
// mémoire : control peut redémarrer au milieu d'un déploiement et reprendre la lecture là où
// l'opérateur en était.
func (m *Manager) Tail(ctx context.Context, id string, w io.Writer, flush func()) error {
	f, err := os.Open(m.LogPath(id))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Le runner n'a pas encore créé le journal : on attend qu'il démarre.
		f, err = m.waitForLog(ctx, id)
		if err != nil {
			return err
		}
	}
	defer f.Close()

	buf := make([]byte, 16<<10)
	idle := time.NewTicker(300 * time.Millisecond)
	defer idle.Stop()
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			flush()
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		job, gerr := m.Get(ctx, id)
		if gerr == nil && job.Done() {
			return nil // plus rien à lire et la tâche est finie
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle.C:
		}
	}
}

func (m *Manager) waitForLog(ctx context.Context, id string) (*os.File, error) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("le runner n'a pas démarré la tâche %s", id)
		case <-t.C:
			if f, err := os.Open(m.LogPath(id)); err == nil {
				return f, nil
			}
			if job, err := m.Get(ctx, id); err == nil && job.Done() {
				return nil, fmt.Errorf("tâche %s terminée sans journal", id)
			}
		}
	}
}
