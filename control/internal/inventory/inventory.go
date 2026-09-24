// Package inventory : l'inventaire Ansible vit en base, le fichier YAML en est la projection.
//
// Ce sens de lecture est le cœur de l'étape « control devient central ». Un opérateur déclare un
// nœud depuis l'interface ; control écrit la ligne, régénère le YAML et le joint à la tâche. Le
// runner ne connaît aucun inventaire : il exécute ce qu'on lui donne. Le mode fichier
// (inventory/*.yml joué depuis un poste) reste valable et n'est pas touché — il sert l'amorçage.
package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Rôles, tels que les groupes de l'inventaire.
const (
	RoleEdge  = "edge"
	RoleQueue = "queue"
	RoleData  = "data"
)

type Node struct {
	Name       string     `json:"name"`
	Roles      []string   `json:"roles"`
	MgmtIP     string     `json:"mgmt_ip"`
	MgmtIP6    string     `json:"mgmt_ip6,omitempty"`
	State      string     `json:"state"`
	Note       string     `json:"note,omitempty"`
	EnrolledAt *time.Time `json:"enrolled_at,omitempty"`
}

type Neighbor struct {
	IP       string `json:"ip"`
	RemoteAS int    `json:"remote_as"`
	Family   string `json:"family"`
}

type Settings struct {
	Env              string         `json:"env"`
	AnsibleUser      string         `json:"ansible_user"`
	WaitHost         string         `json:"wait_host"`
	ControlHost      string         `json:"control_host"`
	VIPAppV4         string         `json:"vip_app_v4"`
	VIPAppV6         string         `json:"vip_app_v6"`
	VIPWaitV4        string         `json:"vip_wait_v4"`
	VIPWaitV6        string         `json:"vip_wait_v6"`
	VIPGatewayV4     string         `json:"vip_gateway_v4"`
	VIPGatewayV6     string         `json:"vip_gateway_v6"`
	BGPEnabled       bool           `json:"bgp_enabled"`
	BGPLocalAS       int            `json:"bgp_local_as"`
	BGPNeighbors     []Neighbor     `json:"bgp_neighbors"`
	ACMEEmail        string         `json:"acme_email"`
	ACMEDirectory    string         `json:"acme_directory"`
	ControlPublicURL string         `json:"control_public_url"`
	CookieSecure     bool           `json:"cookie_secure"`
	ImageTag         string         `json:"image_tag"`
	Registry         string         `json:"registry"`
	QueuePort        int            `json:"queue_port"`
	ControlPort      int            `json:"control_port"`
	DataplanePort    int            `json:"dataplane_port"`
	ManageFirewall   bool           `json:"manage_firewall"`
	DeployFrom       string         `json:"deploy_from"`
	RegistryCA       string         `json:"registry_ca,omitempty"`
	ExtraVars        map[string]any `json:"extra_vars"`
	SeededFromEnv    bool           `json:"seeded_from_env"`
}

const colonnes = `env, ansible_user, wait_host, control_host, vip_app_v4, vip_app_v6,
	vip_wait_v4, vip_wait_v6, vip_gateway_v4, vip_gateway_v6, bgp_enabled, bgp_local_as,
	bgp_neighbors, acme_email, acme_directory, control_public_url, cookie_secure, image_tag,
	queue_port, control_port, dataplane_port, manage_firewall, deploy_from, registry_ca,
	extra_vars, seeded_from_env, registry`

// LoadSettings lit la ligne unique de réglages.
func LoadSettings(ctx context.Context, pool *pgxpool.Pool) (*Settings, error) {
	var s Settings
	var voisins, extra []byte
	err := pool.QueryRow(ctx, `select `+colonnes+` from platform_settings where id=1`).Scan(
		&s.Env, &s.AnsibleUser, &s.WaitHost, &s.ControlHost, &s.VIPAppV4, &s.VIPAppV6,
		&s.VIPWaitV4, &s.VIPWaitV6, &s.VIPGatewayV4, &s.VIPGatewayV6, &s.BGPEnabled, &s.BGPLocalAS,
		&voisins, &s.ACMEEmail, &s.ACMEDirectory, &s.ControlPublicURL, &s.CookieSecure, &s.ImageTag,
		&s.QueuePort, &s.ControlPort, &s.DataplanePort, &s.ManageFirewall, &s.DeployFrom,
		&s.RegistryCA, &extra, &s.SeededFromEnv, &s.Registry)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(voisins, &s.BGPNeighbors); err != nil {
		return nil, fmt.Errorf("voisins BGP : %w", err)
	}
	if err := json.Unmarshal(extra, &s.ExtraVars); err != nil {
		return nil, fmt.Errorf("réglages libres : %w", err)
	}
	return &s, nil
}

// LoadNodes liste les nœuds, groupés par rôle puis par nom — l'ordre de l'inventaire généré, pour
// qu'une régénération sans changement produise un fichier identique.
func LoadNodes(ctx context.Context, pool *pgxpool.Pool) ([]Node, error) {
	rows, err := pool.Query(ctx, `select name, roles, mgmt_ip, mgmt_ip6, state, note, enrolled_at
		from nodes order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Name, &n.Roles, &n.MgmtIP, &n.MgmtIP6, &n.State, &n.Note, &n.EnrolledAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrIncomplete : l'inventaire ne peut pas être généré en l'état.
type ErrIncomplete struct{ Raison string }

func (e ErrIncomplete) Error() string { return e.Raison }

// Render produit l'inventaire YAML. Aucune bibliothèque : la forme est fixe et connue, et la
// sortie doit rester lisible pour audit (commentaires, ordre stable). Toutes les valeurs passent
// par yamlStr, qui cite systématiquement — une adresse comme 10.0.0.60 ou un « yes » deviendraient
// sinon un nombre ou un booléen.
func Render(s *Settings, nodes []Node) ([]byte, error) {
	// Un nœud « planned » figure dans l'inventaire : c'est précisément celui qu'on va installer.
	// Une machine multirôle (profil all-in-one) apparaît dans chacun de ses groupes.
	groupes := map[string][]Node{}
	for _, n := range nodes {
		for _, role := range n.Roles {
			groupes[role] = append(groupes[role], n)
		}
	}
	if len(groupes[RoleData]) == 0 {
		return nil, ErrIncomplete{"aucun hôte data : control ne saurait pas où poser la base"}
	}
	if len(groupes[RoleData]) > 1 {
		return nil, ErrIncomplete{"plusieurs hôtes data : control suppose une instance unique"}
	}

	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# Généré par flyc-control le %s depuis la base. Ne pas modifier : toute retouche serait\n",
		time.Now().UTC().Format(time.RFC3339))
	p("# perdue à la génération suivante. La vérité est dans les tables nodes et platform_settings.\n")
	p("all:\n  vars:\n")
	p("    flyc_env: %s\n", yamlStr(s.Env))
	p("    ansible_user: %s\n", yamlStr(s.AnsibleUser))
	p("    ansible_become: true\n")
	p("    flyc_wait_host: %s\n", yamlStr(s.WaitHost))
	p("    flyc_control_host: %s\n", yamlStr(s.ControlHost))
	p("    flyc_vip_app:  { v4: %s, v6: %s }\n", yamlStr(s.VIPAppV4), yamlStr(s.VIPAppV6))
	p("    flyc_vip_wait: { v4: %s, v6: %s }\n", yamlStr(s.VIPWaitV4), yamlStr(s.VIPWaitV6))
	p("    flyc_bgp:\n")
	p("      enabled: %t\n", s.BGPEnabled)
	p("      local_as: %d\n", s.BGPLocalAS)
	if len(s.BGPNeighbors) == 0 {
		p("      neighbors: []\n")
	} else {
		p("      neighbors:\n")
		for _, n := range s.BGPNeighbors {
			p("        - { ip: %s, remote_as: %d, family: %s }\n", yamlStr(n.IP), n.RemoteAS, yamlStr(n.Family))
		}
	}
	p("    flyc_vip_gateway: { v4: %s, v6: %s }\n", yamlStr(s.VIPGatewayV4), yamlStr(s.VIPGatewayV6))
	p("    flyc_acme_email: %s\n", yamlStr(s.ACMEEmail))
	p("    flyc_acme_directory: %s\n", yamlStr(s.ACMEDirectory))
	p("    flyc_control_public_url: %s\n", yamlStr(s.ControlPublicURL))
	p("    flyc_cookie_secure: %t\n", s.CookieSecure)
	p("    flyc_image_tag: %s\n", yamlStr(s.ImageTag))
	// Le registre est un réglage : une plateforme installée ailleurs ne tire pas ses images d'ici.
	p("    flyc_image_queue: \"%s/flyc-queue:{{ flyc_image_tag }}\"\n", s.Registry)
	p("    flyc_image_control: \"%s/flyc-control:{{ flyc_image_tag }}\"\n", s.Registry)
	p("    flyc_image_runner: \"%s/flyc-runner:{{ flyc_image_tag }}\"\n", s.Registry)
	p("    flyc_queue_port: %d\n", s.QueuePort)
	p("    flyc_control_port: %d\n", s.ControlPort)
	p("    flyc_dataplane_port: %d\n", s.DataplanePort)
	p("    flyc_manage_firewall: %t\n", s.ManageFirewall)
	p("    flyc_deploy_from: %s\n", yamlStr(s.DeployFrom))
	if s.RegistryCA != "" {
		p("    # Certificat racine du registre, téléversé depuis l'interface (contenu, pas un chemin :\n")
		p("    # le fichier d'origine était sur le poste de l'opérateur).\n")
		// « |- » et non « | » : le bloc YAML ajouterait un saut de ligne final que lookup('file')
		// ne produit pas, et le fichier d'amorçage rendu par Ansible différerait selon l'inventaire
		// joué — une modification à chaque passe, sans rien de modifié.
		p("    flyc_registry_ca: |-\n")
		for _, ligne := range strings.Split(strings.TrimRight(s.RegistryCA, "\n"), "\n") {
			p("      %s\n", ligne)
		}
	}
	for _, k := range clesTriees(s.ExtraVars) {
		v, err := yamlScalar(s.ExtraVars[k])
		if err != nil {
			return nil, fmt.Errorf("réglage libre %q : %w", k, err)
		}
		p("    %s: %s\n", k, v)
	}

	p("  children:\n")
	for _, role := range []string{RoleEdge, RoleQueue, RoleData} {
		liste := groupes[role]
		p("    %s:\n", role)
		if len(liste) == 0 {
			p("      hosts: {}\n")
			continue
		}
		p("      hosts:\n")
		for _, n := range liste {
			p("        %s: { ansible_host: %s, mgmt_ip: %s", n.Name, yamlStr(n.MgmtIP), yamlStr(n.MgmtIP))
			if n.MgmtIP6 != "" {
				p(", mgmt_ip6: %s", yamlStr(n.MgmtIP6))
			}
			p(" }\n")
		}
	}
	// Un nœud en cours de retrait reste joignable — c'est par lui qu'on le démonte — mais ne doit
	// pas être réinstallé par un déploiement lancé entre-temps. edge/site.yml exclut ce groupe ;
	// il est écrit même vide, sinon Ansible avertit à chaque passe que le motif ne correspond à rien.
	p("    draining:\n      hosts:")
	retraits := 0
	for _, n := range nodes {
		if n.State == "draining" {
			if retraits == 0 {
				p("\n")
			}
			p("        %s: {}\n", n.Name)
			retraits++
		}
	}
	if retraits == 0 {
		p(" {}\n")
	}
	return []byte(b.String()), nil
}

func clesTriees(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// yamlStr : chaîne toujours entre guillemets, échappement JSON (compatible YAML pour les scalaires
// double-quotés). Une valeur vide devient "", ce qui est exactement ce qu'attendent les rôles.
func yamlStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// yamlScalar rend un réglage libre. Seuls les scalaires sont acceptés : une structure imbriquée
// demanderait un vrai sérialiseur, et aucun réglage connu n'en a besoin.
func yamlScalar(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return yamlStr(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), nil
		}
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case nil:
		return `""`, nil
	}
	return "", fmt.Errorf("type non supporté (%T) : seuls les scalaires sont acceptés", v)
}

// Generate : l'inventaire courant, prêt à être joint à une tâche.
func Generate(ctx context.Context, pool *pgxpool.Pool) ([]byte, error) {
	s, err := LoadSettings(ctx, pool)
	if err != nil {
		return nil, err
	}
	nodes, err := LoadNodes(ctx, pool)
	if err != nil {
		return nil, err
	}
	return Render(s, nodes)
}

// Edge : un LB tel que le réconciliateur a besoin de le connaître.
type Edge struct {
	Name string
	URL  string
}

// Edges lit les LB depuis la base. Appelé à chaque passe de réconciliation : un LB ajouté depuis
// l'interface est pris en compte sans redémarrer control, ce que la variable d'environnement
// FLYC_EDGES ne permettait pas.
//
// Seuls les nœuds « ready » sont poussés. Un nœud « draining » reste dans l'inventaire — c'est par
// lui qu'on le démonte — mais ne reçoit plus de configuration : on ne pousse pas vers une machine
// qu'on est en train d'éteindre.
func Edges(ctx context.Context, pool *pgxpool.Pool) ([]Edge, error) {
	var port int
	if err := pool.QueryRow(ctx, `select dataplane_port from platform_settings where id=1`).Scan(&port); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `select name, mgmt_ip from nodes where 'edge' = any(roles) and state = 'ready' order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var name, ip string
		if err := rows.Scan(&name, &ip); err != nil {
			return nil, err
		}
		out = append(out, Edge{Name: name, URL: fmt.Sprintf("http://%s:%d", hostPort(ip), port)})
	}
	return out, rows.Err()
}

// QueueHosts : « http://adresse:port », la forme attendue par queueclient.
func QueueHosts(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var port int
	if err := pool.QueryRow(ctx, `select queue_port from platform_settings where id=1`).Scan(&port); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `select mgmt_ip from nodes where 'queue' = any(roles) and state = 'ready' order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("http://%s:%d", hostPort(ip), port))
	}
	return out, rows.Err()
}

// hostPort met une adresse IPv6 entre crochets ; une IPv4 ou un nom passent tels quels.
func hostPort(ip string) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		return "[" + ip + "]"
	}
	return ip
}

// Sync recopie nodes vers edges et queue_hosts, les registres d'état lus par l'interface. Seuls
// les nœuds « ready » y figurent : un nœud « planned » n'est pas encore installé et un nœud
// « draining » est en cours de retrait ; dans les deux cas le réconciliateur échouerait à chaque
// passe et l'interface les afficherait en erreur. Les lignes dont le nœud a
// disparu sont supprimées, sinon l'interface garderait un LB fantôme.
func Sync(ctx context.Context, pool *pgxpool.Pool) error {
	// Un inventaire vide n'est pas un ordre de tout effacer : c'est le symptôme d'une base pas
	// encore amorcée. Sans ce garde-fou, démarrer control sans fichier d'amorçage viderait edges
	// et queue_hosts, et la plateforme cesserait de recevoir sa configuration.
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from nodes`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrIncomplete{"aucun nœud déclaré : la topologie n'est pas modifiée"}
	}
	edges, err := Edges(ctx, pool)
	if err != nil {
		return err
	}
	noms := make([]string, 0, len(edges))
	for _, e := range edges {
		if _, err := pool.Exec(ctx, `insert into edges(name, dataplane_url) values($1,$2)
			on conflict (name) do update set dataplane_url = excluded.dataplane_url`, e.Name, e.URL); err != nil {
			return err
		}
		noms = append(noms, e.Name)
	}
	if _, err := pool.Exec(ctx, `delete from edges where name <> all($1::text[])`, noms); err != nil {
		return err
	}

	var qport int
	if err := pool.QueryRow(ctx, `select queue_port from platform_settings where id=1`).Scan(&qport); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `
		insert into queue_hosts(name, address, queue_port)
		select n.name, n.mgmt_ip, $1 from nodes n where 'queue' = any(n.roles) and n.state = 'ready'
		on conflict (name) do update set address = excluded.address, queue_port = excluded.queue_port`, qport); err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `delete from queue_hosts where name not in
		(select name from nodes where 'queue' = any(roles) and state = 'ready')`)
	return err
}
