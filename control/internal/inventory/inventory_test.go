package inventory

import (
	"strings"
	"testing"
)

func reglages() *Settings {
	return &Settings{
		Env: "test", AnsibleUser: "flyc-deploy",
		WaitHost: "wait.example.com", ControlHost: "control.example.com",
		VIPAppV4: "203.0.113.7", VIPAppV6: "2001:db8::7",
		VIPWaitV4: "203.0.113.8", VIPWaitV6: "2001:db8::8",
		VIPGatewayV4: "10.0.0.254",
		BGPEnabled:   true, BGPLocalAS: 64496,
		BGPNeighbors: []Neighbor{
			{IP: "10.0.0.254", RemoteAS: 64497, Family: "v4"},
			{IP: "fd00::1", RemoteAS: 64497, Family: "v6"},
		},
		ACMEEmail: "admin@example.com", ControlPublicURL: "http://control.example.com:8090",
		ImageTag: "e136f7b2", Registry: "registry.example.com/flyc", QueuePort: 8080, ControlPort: 8090, DataplanePort: 5555,
		ManageFirewall: true, DeployFrom: "10.0.0.65",
		ExtraVars: map[string]any{"haproxy_ticket_rate_limit": float64(100000)},
	}
}

func nodes() []Node {
	return []Node{
		{Name: "lb0", Roles: []string{RoleEdge}, MgmtIP: "10.0.0.60", MgmtIP6: "fd00::60", State: "ready"},
		{Name: "queue0", Roles: []string{RoleQueue}, MgmtIP: "10.0.0.62", State: "ready"},
		{Name: "control0", Roles: []string{RoleData}, MgmtIP: "10.0.0.65", State: "ready"},
	}
}

func TestRender(t *testing.T) {
	y, err := Render(reglages(), nodes())
	if err != nil {
		t.Fatal(err)
	}
	out := string(y)
	for _, attendu := range []string{
		`    flyc_vip_app:  { v4: "203.0.113.7", v6: "2001:db8::7" }`,
		`      local_as: 64496`,
		`        - { ip: "fd00::1", remote_as: 64497, family: "v6" }`,
		`    flyc_vip_gateway: { v4: "10.0.0.254", v6: "" }`,
		`    flyc_image_queue: "registry.example.com/flyc/flyc-queue:{{ flyc_image_tag }}"`,
		`    haproxy_ticket_rate_limit: 100000`,
		`        lb0: { ansible_host: "10.0.0.60", mgmt_ip: "10.0.0.60", mgmt_ip6: "fd00::60" }`,
		`        queue0: { ansible_host: "10.0.0.62", mgmt_ip: "10.0.0.62" }`,
		`    data:`,
	} {
		if !strings.Contains(out, attendu) {
			t.Errorf("ligne absente de l'inventaire généré :\n%s\n--- rendu ---\n%s", attendu, out)
		}
	}
}

// Sans hôte data, l'inventaire n'a pas de sens : mieux vaut un refus lisible qu'un playbook qui
// échoue sur groups['data'][0] au milieu d'un déploiement.
func TestRenderSansData(t *testing.T) {
	var sansData []Node
	for _, n := range nodes() {
		if n.Roles[0] != RoleData {
			sansData = append(sansData, n)
		}
	}
	if _, err := Render(reglages(), sansData); err == nil {
		t.Fatal("un inventaire sans hôte data devrait être refusé")
	}
}

// Un nœud en cours de retrait reste joignable mais sort des plays d'installation.
func TestRenderDraining(t *testing.T) {
	n := nodes()
	n[1].State = "draining"
	y, err := Render(reglages(), n)
	if err != nil {
		t.Fatal(err)
	}
	out := string(y)
	if !strings.Contains(out, "    draining:\n      hosts:\n        queue0: {}\n") {
		t.Errorf("groupe draining absent :\n%s", out)
	}
	if !strings.Contains(out, `queue0: { ansible_host: "10.0.0.62"`) {
		t.Error("le nœud doit rester dans son groupe de rôle, pour qu'on puisse le démonter")
	}
}

// Sans nœud en retrait, le groupe est écrit vide : Ansible avertirait sinon à chaque passe.
func TestRenderDrainingVide(t *testing.T) {
	y, _ := Render(reglages(), nodes())
	if !strings.Contains(string(y), "    draining:\n      hosts: {}\n") {
		t.Errorf("groupe draining vide mal rendu :\n%s", y)
	}
}

// Un groupe vide doit rester un groupe : « edge: » sans clé hosts casse le chargement Ansible.
func TestRenderGroupeVide(t *testing.T) {
	y, err := Render(reglages(), []Node{{Name: "control0", Roles: []string{RoleData}, MgmtIP: "10.0.0.65"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "    edge:\n      hosts: {}\n") {
		t.Errorf("groupe edge vide mal rendu :\n%s", y)
	}
}

// Les valeurs sont toujours citées : sans cela « 10.0.0.60 » reste une chaîne mais « yes »,
// « on » ou « 0755 » changeraient de type au chargement.
// Le certificat est rendu en bloc « |- » : un saut de ligne final ferait diverger le fichier
// d'amorçage selon l'inventaire joué, donc une modification signalée à chaque passe.
func TestRenderCertificatSansSautFinal(t *testing.T) {
	s := reglages()
	s.RegistryCA = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----"
	y, err := Render(s, nodes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(y), "flyc_registry_ca: |-\n      -----BEGIN") {
		t.Errorf("bloc du certificat mal rendu :\n%s", y)
	}
}

func TestQuotage(t *testing.T) {
	for _, cas := range []struct{ in, out string }{
		{"yes", `"yes"`},
		{"", `""`},
		{`a"b`, `"a\"b"`},
	} {
		if got := yamlStr(cas.in); got != cas.out {
			t.Errorf("yamlStr(%q) = %s, attendu %s", cas.in, got, cas.out)
		}
	}
	if _, err := yamlScalar(map[string]any{"a": 1}); err == nil {
		t.Error("un réglage libre imbriqué devrait être refusé")
	}
}

// Une machine all-in-one porte les trois rôles et doit apparaître dans les trois groupes : c'est
// la forme que prend l'inventaire d'une plateforme sur une seule machine.
func TestRenderMultirole(t *testing.T) {
	y, err := Render(reglages(), []Node{
		{Name: "box", Roles: []string{RoleEdge, RoleQueue, RoleData}, MgmtIP: "10.0.0.1", State: "ready"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(y), `box: { ansible_host: "10.0.0.1"`); n != 3 {
		t.Errorf("box devrait figurer dans les trois groupes, vu %d fois :\n%s", n, y)
	}
}
