package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/flyc-io/flyc/control/internal/inventory"
	"github.com/flyc-io/flyc/control/internal/jobs"
	"github.com/flyc-io/flyc/internal/httpx"
)

// Les opérations d'infrastructure sont réservées à la plateforme : elles touchent les machines,
// pas la configuration d'un client.
func (a *API) registerJobs(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/jobs", a.platform(a.createJob))
	mux.HandleFunc("GET /v1/jobs", a.platform(a.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", a.platform(a.getJob))
	mux.HandleFunc("GET /v1/jobs/{id}/log", a.platform(a.jobLog))
}

type jobInput struct {
	Kind    string       `json:"kind"`
	Request jobs.Request `json:"request"`
}

func (a *API) createJob(w http.ResponseWriter, r *http.Request, p *principal) {
	if a.jobs == nil {
		httpx.Error(w, 503, "no_runner", "aucun runner configuré sur cet hôte")
		return
	}
	var in jobInput
	if !decode(w, r, &in) {
		return
	}
	if in.Kind == "" {
		httpx.Error(w, 400, "invalid", "kind requis")
		return
	}
	// L'inventaire est généré depuis la base : l'appelant n'a rien à fournir, et ne peut donc pas
	// faire exécuter une topologie qui n'est pas celle déclarée. Il reste possible d'en joindre un
	// explicitement — c'est ce que fait l'amorçage, avant que la base ne soit peuplée.
	if in.Request.Inventory == "" {
		y, err := inventory.Generate(r.Context(), a.pool)
		if err != nil {
			var inc inventory.ErrIncomplete
			if errors.As(err, &inc) {
				httpx.Error(w, 409, "incomplete", inc.Raison)
				return
			}
			dbErr(w, err)
			return
		}
		in.Request.Inventory = string(y)
	}
	job, err := a.jobs.Submit(r.Context(), in.Kind, in.Request, actor(p))
	if errors.Is(err, jobs.ErrBusy) {
		httpx.Error(w, 409, "busy", "une tâche est déjà en cours : attendre sa fin")
		return
	}
	if err != nil {
		a.log.Error("dépôt de tâche", "err", err)
		httpx.Error(w, 500, "job", err.Error())
		return
	}
	a.audit(r.Context(), p, "job.create", map[string]string{"id": job.ID, "kind": in.Kind})
	httpx.JSON(w, 201, job)
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request, _ *principal) {
	if a.jobs == nil {
		httpx.JSON(w, 200, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.jobs.List(r.Context(), limit)
	if err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, list)
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request, _ *principal) {
	if a.jobs == nil {
		httpx.Error(w, 404, "not_found", "aucune tâche")
		return
	}
	job, err := a.jobs.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, 404, "not_found", "tâche inconnue")
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, job)
}

// jobLog diffuse le journal au fil de l'eau. Un déploiement dure des minutes : l'opérateur doit
// voir ce qui se passe, pas attendre un verdict.
func (a *API) jobLog(w http.ResponseWriter, r *http.Request, _ *principal) {
	if a.jobs == nil {
		httpx.Error(w, 404, "not_found", "aucune tâche")
		return
	}
	id := r.PathValue("id")
	if _, err := a.jobs.Get(r.Context(), id); err != nil {
		httpx.Error(w, 404, "not_found", "tâche inconnue")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, 500, "no_stream", "diffusion impossible")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	if err := a.jobs.Tail(r.Context(), id, &sseWriter{w: w}, flusher.Flush); err != nil &&
		r.Context().Err() == nil {
		a.log.Warn("diffusion du journal", "id", id, "err", err)
	}
	_, _ = w.Write([]byte("event: end\ndata: {}\n\n"))
	flusher.Flush()
}

// sseWriter emballe des octets bruts en événements SSE, une ligne par événement.
type sseWriter struct {
	w    http.ResponseWriter
	rest []byte
}

func (s *sseWriter) Write(p []byte) (int, error) {
	n := len(p)
	s.rest = append(s.rest, p...)
	for {
		i := indexByte(s.rest, '\n')
		if i < 0 {
			break
		}
		line := s.rest[:i]
		s.rest = s.rest[i+1:]
		if _, err := s.w.Write([]byte("data: ")); err != nil {
			return n, err
		}
		if _, err := s.w.Write(line); err != nil {
			return n, err
		}
		if _, err := s.w.Write([]byte("\n\n")); err != nil {
			return n, err
		}
	}
	return n, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// actor : qui a demandé la tâche, pour la trace.
func actor(p *principal) string {
	if p.user != nil {
		return p.user.Email
	}
	if p.keyID != "" {
		return "clé " + p.keyID
	}
	return "inconnu"
}
