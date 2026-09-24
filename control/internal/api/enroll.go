package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flyc-io/flyc/control/internal/inventory"
	"github.com/flyc-io/flyc/internal/httpx"
)

// Enrôlement d'une machine vierge.
//
// L'opérateur crée un jeton, l'interface affiche une ligne à coller sur la machine, et le script
// servi par control y crée le compte de déploiement avec la clé publique. Rien de secret ne
// circule : le script ne transporte qu'une clé publique, et le jeton — à usage unique, valable
// quinze minutes — ne sert qu'à lier la machine à une demande.
func (a *API) registerEnroll(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/enroll/tokens", a.platform(a.createEnrollToken))
	mux.HandleFunc("GET /v1/enroll/tokens", a.platform(a.listEnrollTokens))
	mux.HandleFunc("DELETE /v1/enroll/tokens/{id}", a.platform(a.deleteEnrollToken))
	// Sans authentification : le jeton est la seule preuve, et la machine qui l'exécute n'a par
	// définition aucun compte sur la plateforme.
	mux.HandleFunc("GET /enroll/{token}", a.enrollScript)
	mux.HandleFunc("POST /enroll/{token}/done", a.enrollDone)
}

const enrollTTL = 15 * time.Minute

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// deployPubkey : la moitié publique de la clé de déploiement. Seul ce fichier est monté dans
// control — la clé privée ne l'est pas, et ne doit pas l'être : c'est le runner qui s'en sert.
func deployPubkey() (string, error) {
	path := os.Getenv("FLYC_DEPLOY_PUBKEY_FILE")
	if path == "" {
		path = "/etc/flyc/deploy.pub"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") {
		return "", fmt.Errorf("%s ne ressemble pas à une clé publique SSH", path)
	}
	return k, nil
}

type enrollInput struct {
	Roles []string `json:"roles"`
}

func (a *API) createEnrollToken(w http.ResponseWriter, r *http.Request, p *principal) {
	var in enrollInput
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	if _, err := deployPubkey(); err != nil {
		httpx.Error(w, 503, "no_deploy_key", "clé de déploiement introuvable : "+err.Error())
		return
	}
	brut := make([]byte, 32)
	if _, err := rand.Read(brut); err != nil {
		httpx.Error(w, 500, "rand", err.Error())
		return
	}
	token := hex.EncodeToString(brut)
	var id string
	var exp time.Time
	err := a.pool.QueryRow(r.Context(),
		`insert into enroll_tokens(token_hash, roles, created_by, expires_at)
		 values($1,$2,$3,now()+$4::interval) returning id::text, expires_at`,
		hashToken(token), in.Roles, actor(p), enrollTTL.String()).Scan(&id, &exp)
	if err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "enroll.token", map[string]any{"id": id, "roles": in.Roles})
	base := a.enrollBase(r)
	httpx.JSON(w, 201, map[string]any{
		"id":         id,
		"expires_at": exp,
		"url":        base + "/enroll/" + token,
		"command":    "curl -fsSL " + base + "/enroll/" + token + " | sudo sh",
	})
}

// enrollBase : l'URL publique de control, celle que la machine à enrôler doit joindre. L'URL
// configurée fait foi ; l'en-tête Host ne sert que si elle est absente.
func (a *API) enrollBase(r *http.Request) string {
	if u := strings.TrimRight(os.Getenv("FLYC_PUBLIC_URL"), "/"); u != "" {
		return u
	}
	schema := "https"
	if r.TLS == nil {
		schema = "http"
	}
	return schema + "://" + r.Host
}

func (a *API) listEnrollTokens(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select id::text, roles, created_by, created_at,
		expires_at, used_at, hostname, mgmt_ip, mgmt_ip6 from enroll_tokens
		where used_at is not null or expires_at > now() order by created_at desc limit 50`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, by, host, ip, ip6 string
		var roles []string
		var created, exp time.Time
		var used *time.Time
		if err := rows.Scan(&id, &roles, &by, &created, &exp, &used, &host, &ip, &ip6); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, map[string]any{"id": id, "roles": roles, "created_by": by,
			"created_at": created, "expires_at": exp, "used_at": used,
			"hostname": host, "mgmt_ip": ip, "mgmt_ip6": ip6})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) deleteEnrollToken(w http.ResponseWriter, r *http.Request, p *principal) {
	tag, err := a.pool.Exec(r.Context(), `delete from enroll_tokens where id=$1`, r.PathValue("id"))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "aucun jeton de ce nom")
		return
	}
	a.audit(r.Context(), p, "enroll.token.delete", map[string]string{"id": r.PathValue("id")})
	w.WriteHeader(204)
}

// lookupToken : le jeton est valide s'il existe, n'a pas servi et n'a pas expiré.
func (a *API) lookupToken(r *http.Request) (string, error) {
	var id string
	err := a.pool.QueryRow(r.Context(),
		`select id::text from enroll_tokens where token_hash=$1 and used_at is null and expires_at > now()`,
		hashToken(r.PathValue("token"))).Scan(&id)
	return id, err
}

var scriptEnrolement = template.Must(template.New("enroll").Parse(`#!/bin/sh
# Flyc — enrôlement de cette machine.
#
# Crée le compte de déploiement {{.User}}, y installe la clé PUBLIQUE de l'hôte control, puis
# annonce la machine. Rien d'autre : l'installation des paquets et des services viendra ensuite,
# par Ansible, depuis control. Aucun secret ne figure dans ce script.
set -eu

USER={{.User}}
HOME_DIR=/var/lib/$USER
CONTROL='{{.Base}}'
TOKEN='{{.Token}}'
CLE='{{.Options}}{{.Key}}'

[ "$(id -u)" = 0 ] || { echo "à lancer en root : curl -fsSL … | sudo sh" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl est requis" >&2; exit 1; }

id "$USER" >/dev/null 2>&1 || useradd --system --shell /bin/bash --home-dir "$HOME_DIR" --create-home "$USER"
# Aucun mot de passe : ce compte ne s'ouvre qu'avec la clé.
passwd -l "$USER" >/dev/null 2>&1 || true

install -d -m 0700 -o "$USER" -g "$USER" "$HOME_DIR/.ssh"
printf '%s\n' "$CLE" > "$HOME_DIR/.ssh/authorized_keys"
chmod 0600 "$HOME_DIR/.ssh/authorized_keys"
chown "$USER:$USER" "$HOME_DIR/.ssh/authorized_keys"

# Configurer une machine suppose d'installer des paquets et d'écrire dans /etc : une liste blanche
# serait un décor. Les restrictions utiles portent sur la clé, ci-dessus, pas sur cette ligne.
cat > /etc/sudoers.d/$USER <<EOF
# Écrit à l'enrôlement, puis maintenu par Ansible (rôle common) : ne pas modifier à la main.
$USER ALL=(root) NOPASSWD: ALL
Defaults:$USER !requiretty
EOF
chmod 0440 /etc/sudoers.d/$USER
visudo -cf /etc/sudoers.d/$USER >/dev/null

# Adresses annoncées : celles par lesquelles cette machine joint control, donc celles par
# lesquelles control la joindra. Lecture seule, aucune configuration réseau n'est touchée.
CIBLE='{{.Probe}}'
IP4=$(ip -4 route get "$CIBLE" 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
[ -n "$IP4" ] || IP4=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
# Une adresse construite sur la MAC (ff:fe au milieu) ou temporaire est rarement celle qu'on veut
# voir dans un inventaire : on préfère une adresse posée à la main quand il y en a une.
IP6=$(ip -6 -o addr show scope global 2>/dev/null | awk '$2 != "lo" {print $4}' | cut -d/ -f1 | grep -v 'ff:fe' | head -1)
[ -n "$IP6" ] || IP6=$(ip -6 -o addr show scope global 2>/dev/null | awk '$2 != "lo" {print $4}' | cut -d/ -f1 | head -1)
HOTE=$(hostname -s)

echo "flyc : compte $USER prêt sur $HOTE ($IP4${IP6:+, $IP6})"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"hostname\":\"$HOTE\",\"mgmt_ip\":\"$IP4\",\"mgmt_ip6\":\"${IP6:-}\"}" \
  "$CONTROL/enroll/$TOKEN/done" >/dev/null
echo "flyc : machine annoncée à control, elle apparaît dans l'interface"
`))

func (a *API) enrollScript(w http.ResponseWriter, r *http.Request) {
	if _, err := a.lookupToken(r); err != nil {
		// Volontairement avare : un jeton inconnu, expiré ou déjà servi donnent la même réponse.
		http.Error(w, "# jeton inconnu, expiré ou déjà utilisé\nexit 1\n", 404)
		return
	}
	key, err := deployPubkey()
	if err != nil {
		http.Error(w, "# clé de déploiement indisponible sur l'hôte control\nexit 1\n", 503)
		return
	}
	s, err := inventory.LoadSettings(r.Context(), a.pool)
	if err != nil {
		http.Error(w, "# réglages illisibles\nexit 1\n", 500)
		return
	}
	options := "no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-user-rc "
	if s.DeployFrom != "" {
		options = `from="` + s.DeployFrom + `",` + options
	}
	probe := s.DeployFrom
	if probe == "" || strings.Contains(probe, ",") {
		probe = "1.1.1.1"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = scriptEnrolement.Execute(w, map[string]string{
		"User": s.AnsibleUser, "Base": a.enrollBase(r), "Token": r.PathValue("token"),
		"Key": key, "Options": options, "Probe": probe,
	})
}

type enrollDoneInput struct {
	Hostname string `json:"hostname"`
	MgmtIP   string `json:"mgmt_ip"`
	MgmtIP6  string `json:"mgmt_ip6"`
}

// enrollDone consomme le jeton. Le nœud n'est pas créé ici : il n'a pas encore de rôle, et c'est à
// l'opérateur de le lui donner dans l'interface. La machine est simplement enregistrée comme
// candidate, avec ce qu'elle a annoncé.
func (a *API) enrollDone(w http.ResponseWriter, r *http.Request) {
	id, err := a.lookupToken(r)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, 404, "not_found", "jeton inconnu, expiré ou déjà utilisé")
		return
	}
	if err != nil {
		dbErr(w, err)
		return
	}
	var in enrollDoneInput
	if !decode(w, r, &in) {
		return
	}
	if net.ParseIP(in.MgmtIP) == nil {
		httpx.Error(w, 400, "invalid", "mgmt_ip n'est pas une adresse IP")
		return
	}
	if in.MgmtIP6 != "" {
		if ip := net.ParseIP(in.MgmtIP6); ip == nil || ip.To4() != nil {
			in.MgmtIP6 = "" // une adresse douteuse ne vaut pas mieux que pas d'adresse
		}
	}
	if _, err := a.pool.Exec(r.Context(), `update enroll_tokens
		set used_at=now(), hostname=$2, mgmt_ip=$3, mgmt_ip6=$4 where id=$1`,
		id, in.Hostname, in.MgmtIP, in.MgmtIP6); err != nil {
		dbErr(w, err)
		return
	}
	a.log.Info("machine enrôlée", "hôte", in.Hostname, "ip", in.MgmtIP)
	httpx.JSON(w, 200, map[string]string{"status": "enrolled", "hostname": in.Hostname})
}
