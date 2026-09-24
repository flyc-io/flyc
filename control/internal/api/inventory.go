package api

import (
	"errors"
	"net"
	"net/http"
	"regexp"

	"github.com/flyc-io/flyc/control/internal/inventory"
	"github.com/flyc-io/flyc/internal/httpx"
)

// L'inventaire est une donnée de la plateforme : ces routes sont le seul chemin pour le modifier
// depuis l'interface. Le fichier YAML n'en est qu'une projection, régénérée à chaque tâche.
func (a *API) registerInventory(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/nodes", a.platform(a.listNodes))
	mux.HandleFunc("POST /v1/nodes", a.platform(a.createNode))
	mux.HandleFunc("PATCH /v1/nodes/{name}", a.platform(a.patchNode))
	mux.HandleFunc("DELETE /v1/nodes/{name}", a.platform(a.deleteNode))
	mux.HandleFunc("GET /v1/settings", a.platform(a.getSettings))
	mux.HandleFunc("PUT /v1/settings", a.platform(a.putSettings))
	mux.HandleFunc("GET /v1/inventory", a.platform(a.getInventory))
}

func porte(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

var nomNode = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// valideNode refuse ce qui casserait l'inventaire généré plus tard, au moment le plus coûteux :
// un nom qui n'est pas un nom d'hôte Ansible, une adresse qui n'en est pas une.
func valideNode(n *inventory.Node) string {
	if !nomNode.MatchString(n.Name) {
		return "nom invalide : minuscules, chiffres, point, tiret et souligné"
	}
	if len(n.Roles) == 0 {
		return "au moins un rôle est requis : edge, queue ou data"
	}
	vus := map[string]bool{}
	for _, role := range n.Roles {
		switch role {
		case inventory.RoleEdge, inventory.RoleQueue, inventory.RoleData:
		default:
			return "rôle inconnu : edge, queue ou data"
		}
		if vus[role] {
			return "rôle en double : " + role
		}
		vus[role] = true
	}
	if net.ParseIP(n.MgmtIP) == nil {
		return "mgmt_ip n'est pas une adresse IP"
	}
	if n.MgmtIP6 != "" {
		ip := net.ParseIP(n.MgmtIP6)
		if ip == nil || ip.To4() != nil {
			return "mgmt_ip6 n'est pas une adresse IPv6"
		}
	}
	return ""
}

func (a *API) listNodes(w http.ResponseWriter, r *http.Request, _ *principal) {
	nodes, err := inventory.LoadNodes(r.Context(), a.pool)
	if err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, nodes)
}

func (a *API) createNode(w http.ResponseWriter, r *http.Request, p *principal) {
	var n inventory.Node
	if !decode(w, r, &n) {
		return
	}
	if msg := valideNode(&n); msg != "" {
		httpx.Error(w, 400, "invalid", msg)
		return
	}
	// L'hôte data porte control et la base : le code suppose une instance unique, et un second
	// hôte data rendrait l'inventaire ingénérable au premier déploiement.
	if porte(n.Roles, inventory.RoleData) {
		var dejaLa int
		_ = a.pool.QueryRow(r.Context(), `select count(*) from nodes where 'data' = any(roles)`).Scan(&dejaLa)
		if dejaLa > 0 {
			httpx.Error(w, 409, "conflict", "un hôte data existe déjà : control suppose une instance unique")
			return
		}
	}
	if n.State == "" {
		n.State = "planned"
	}
	if err := inventory.SaveNode(r.Context(), a.pool, &n); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "node.create", n)
	a.rec.Kick()
	httpx.JSON(w, 201, n)
}

type nodePatch struct {
	MgmtIP  *string `json:"mgmt_ip"`
	MgmtIP6 *string `json:"mgmt_ip6"`
	State   *string `json:"state"`
	Note    *string `json:"note"`
}

func (a *API) patchNode(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	nodes, err := inventory.LoadNodes(r.Context(), a.pool)
	if err != nil {
		dbErr(w, err)
		return
	}
	var cur *inventory.Node
	for i := range nodes {
		if nodes[i].Name == name {
			cur = &nodes[i]
		}
	}
	if cur == nil {
		httpx.Error(w, 404, "not_found", "aucun nœud de ce nom")
		return
	}
	var in nodePatch
	if !decode(w, r, &in) {
		return
	}
	if in.MgmtIP != nil {
		cur.MgmtIP = *in.MgmtIP
	}
	if in.MgmtIP6 != nil {
		cur.MgmtIP6 = *in.MgmtIP6
	}
	if in.State != nil {
		cur.State = *in.State
	}
	if in.Note != nil {
		cur.Note = *in.Note
	}
	if msg := valideNode(cur); msg != "" {
		httpx.Error(w, 400, "invalid", msg)
		return
	}
	if err := inventory.SaveNode(r.Context(), a.pool, cur); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "node.update", cur)
	a.rec.Kick()
	httpx.JSON(w, 200, cur)
}

// deleteNode sort un nœud de l'inventaire. Il ne démonte rien : un nœud encore en service doit
// d'abord être mis hors rotation (tools/add-node.sh remove, ou la tâche équivalente), sans quoi
// la machine continuerait de tourner en croyant faire partie de la plateforme.
func (a *API) deleteNode(w http.ResponseWriter, r *http.Request, p *principal) {
	name := r.PathValue("name")
	var roles []string
	var state string
	if err := a.pool.QueryRow(r.Context(), `select roles, state from nodes where name=$1`, name).Scan(&roles, &state); err != nil {
		dbErr(w, err)
		return
	}
	if porte(roles, inventory.RoleData) {
		httpx.Error(w, 409, "still_configured", "l'hôte data porte control et la base : il ne se retire pas depuis l'interface")
		return
	}
	if state == "ready" {
		httpx.Error(w, 409, "still_configured", "ce nœud est en service : le mettre en retrait (draining) et le décommissionner d'abord")
		return
	}
	if _, err := a.pool.Exec(r.Context(), `delete from nodes where name=$1`, name); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "node.delete", map[string]any{"name": name, "roles": roles})
	a.rec.Kick()
	w.WriteHeader(204)
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, _ *principal) {
	s, err := inventory.LoadSettings(r.Context(), a.pool)
	if err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, s)
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request, p *principal) {
	cur, err := inventory.LoadSettings(r.Context(), a.pool)
	if err != nil {
		dbErr(w, err)
		return
	}
	// Décodage par-dessus l'existant : un champ omis garde sa valeur, ce qui évite qu'une
	// interface partielle vide silencieusement la moitié des réglages.
	if !decode(w, r, cur) {
		return
	}
	if err := inventory.SaveSettings(r.Context(), a.pool, cur, false); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "settings.update", cur)
	a.rec.Kick()
	httpx.JSON(w, 200, cur)
}

// getInventory rend l'inventaire tel qu'il sera joint aux tâches : c'est la pièce d'audit qui
// permet de vérifier ce que control demandera au runner, sans lancer de tâche.
func (a *API) getInventory(w http.ResponseWriter, r *http.Request, _ *principal) {
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
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	_, _ = w.Write(y)
}
