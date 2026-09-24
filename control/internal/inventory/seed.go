package inventory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed : l'inventaire tel qu'Ansible le connaît, transmis à control dans FLYC_INVENTORY_SEED.
//
// Il ne sert qu'une fois, au tout premier démarrage : il amorce la base depuis le fichier qui a
// servi à installer la plateforme. Ensuite la base fait foi, et un écart entre les deux est
// signalé dans le journal sans jamais être appliqué — sinon un nœud retiré depuis l'interface
// réapparaîtrait au premier redéploiement fait depuis un poste.
type Seed struct {
	Settings Settings `json:"settings"`
	Nodes    []Node   `json:"nodes"`
}

// Lire renvoie l'amorce, depuis un fichier si FLYC_INVENTORY_SEED_FILE est posé, sinon depuis la
// variable elle-même. Un fichier absent n'est pas une erreur : c'est le cas normal d'une plateforme
// dont la base est déjà peuplée, et d'un montage bind que docker a créé en répertoire vide.
func Lire(fichier, inline string) string {
	if fichier == "" {
		return inline
	}
	b, err := os.ReadFile(fichier)
	if err != nil {
		return inline
	}
	return string(b)
}

// Apply amorce la base si elle est vide, sinon compare et signale.
func Apply(ctx context.Context, pool *pgxpool.Pool, raw string, log *slog.Logger) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var s Seed
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return fmt.Errorf("FLYC_INVENTORY_SEED illisible : %w", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from nodes`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		signalerEcart(ctx, pool, s, log)
		return nil
	}
	if len(s.Nodes) == 0 {
		return nil
	}
	if err := SaveSettings(ctx, pool, &s.Settings, true); err != nil {
		return err
	}
	for _, node := range s.Nodes {
		// Les nœuds décrits par l'inventaire d'installation sont déjà en service.
		if node.State == "" {
			node.State = "ready"
		}
		if err := SaveNode(ctx, pool, &node); err != nil {
			return fmt.Errorf("amorçage du nœud %s : %w", node.Name, err)
		}
	}
	log.Info("inventaire amorcé depuis l'environnement", "nœuds", len(s.Nodes))
	return Sync(ctx, pool)
}

func signalerEcart(ctx context.Context, pool *pgxpool.Pool, s Seed, log *slog.Logger) {
	nodes, err := LoadNodes(ctx, pool)
	if err != nil {
		return
	}
	base := map[string]string{}
	for _, n := range nodes {
		base[n.Name] = strings.Join(n.Roles, "+") + " " + n.MgmtIP
	}
	env := map[string]string{}
	for _, n := range s.Nodes {
		env[n.Name] = strings.Join(n.Roles, "+") + " " + n.MgmtIP
	}
	var ecarts []string
	for _, nom := range clesUnion(base, env) {
		switch {
		case base[nom] == env[nom]:
		case base[nom] == "":
			ecarts = append(ecarts, nom+" absent de la base")
		case env[nom] == "":
			ecarts = append(ecarts, nom+" absent du fichier")
		default:
			ecarts = append(ecarts, fmt.Sprintf("%s : base %q, fichier %q", nom, base[nom], env[nom]))
		}
	}
	ecarts = append(ecarts, ecartsReglages(ctx, pool, s.Settings)...)
	if len(ecarts) > 0 {
		log.Warn("l'inventaire fichier diverge de la base, qui fait foi", "écarts", strings.Join(ecarts, " ; "))
	}
}

// ecartsReglages compare les réglages du fichier à ceux de la base. Le cas qui mord en pratique est
// image_tag : une version déployée depuis un poste ne change que le fichier, et la tâche suivante
// lancée depuis l'interface rejouerait la version d'avant. Mieux vaut le lire dans le journal que
// le découvrir sur un déploiement.
func ecartsReglages(ctx context.Context, pool *pgxpool.Pool, fichier Settings) []string {
	base, err := LoadSettings(ctx, pool)
	if err != nil {
		return nil
	}
	a, b := champs(*base), champs(fichier)
	var out []string
	for _, k := range clesUnion(a, b) {
		// Le fichier ne porte pas tous les réglages (le certificat du registre peut n'exister que
		// d'un côté) : seule une valeur renseignée des deux côtés et différente est un écart.
		if a[k] != b[k] && a[k] != "" && b[k] != "" {
			out = append(out, fmt.Sprintf("%s : base %q, fichier %q", k, court(a[k]), court(b[k])))
		}
	}
	return out
}

func champs(s Settings) map[string]string {
	s.SeededFromEnv = false
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var brut map[string]any
	if err := json.Unmarshal(b, &brut); err != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range brut {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func court(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

func clesUnion(a, b map[string]string) []string {
	vu := map[string]bool{}
	out := []string{}
	for _, m := range []map[string]string{a, b} {
		for k := range m {
			if !vu[k] {
				vu[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// SaveSettings écrit la ligne unique de réglages.
func SaveSettings(ctx context.Context, pool *pgxpool.Pool, s *Settings, seeded bool) error {
	voisins, err := json.Marshal(s.BGPNeighbors)
	if err != nil {
		return err
	}
	if s.ExtraVars == nil {
		s.ExtraVars = map[string]any{}
	}
	extra, err := json.Marshal(s.ExtraVars)
	if err != nil {
		return err
	}
	if s.AnsibleUser == "" {
		s.AnsibleUser = "flyc-deploy"
	}
	for champ, valeur := range map[string]*int{"queue_port": &s.QueuePort, "control_port": &s.ControlPort, "dataplane_port": &s.DataplanePort} {
		if *valeur == 0 {
			*valeur = defautPort[champ]
		}
	}
	_, err = pool.Exec(ctx, `update platform_settings set
		env=$1, ansible_user=$2, wait_host=$3, control_host=$4, vip_app_v4=$5, vip_app_v6=$6,
		vip_wait_v4=$7, vip_wait_v6=$8, vip_gateway_v4=$9, vip_gateway_v6=$10, bgp_enabled=$11,
		bgp_local_as=$12, bgp_neighbors=$13, acme_email=$14, acme_directory=$15,
		control_public_url=$16, cookie_secure=$17, image_tag=$18, queue_port=$19, control_port=$20,
		dataplane_port=$21, manage_firewall=$22, deploy_from=$23, registry_ca=$24, extra_vars=$25,
		seeded_from_env=$26, registry=coalesce(nullif($27,''), registry), updated_at=now() where id=1`,
		s.Env, s.AnsibleUser, s.WaitHost, s.ControlHost, s.VIPAppV4, s.VIPAppV6,
		s.VIPWaitV4, s.VIPWaitV6, s.VIPGatewayV4, s.VIPGatewayV6, s.BGPEnabled,
		s.BGPLocalAS, voisins, s.ACMEEmail, s.ACMEDirectory,
		s.ControlPublicURL, s.CookieSecure, s.ImageTag, s.QueuePort, s.ControlPort,
		s.DataplanePort, s.ManageFirewall, s.DeployFrom, s.RegistryCA, extra, seeded, s.Registry)
	return err
}

var defautPort = map[string]int{"queue_port": 8080, "control_port": 8090, "dataplane_port": 5555}

// SaveNode crée ou met à jour un nœud.
func SaveNode(ctx context.Context, pool *pgxpool.Pool, n *Node) error {
	if n.State == "" {
		n.State = "planned"
	}
	_, err := pool.Exec(ctx, `insert into nodes(name, roles, mgmt_ip, mgmt_ip6, state, note)
		values($1,$2,$3,$4,$5,$6)
		on conflict (name) do update set roles=excluded.roles, mgmt_ip=excluded.mgmt_ip,
			mgmt_ip6=excluded.mgmt_ip6, state=excluded.state, note=excluded.note, updated_at=now()`,
		n.Name, n.Roles, n.MgmtIP, n.MgmtIP6, n.State, n.Note)
	return err
}
