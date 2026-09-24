// Package compile : lit la base et produit l'état désiré de chaque LB (backends, maps, certificats)
// et de flyc-queue (configuration des files).
package compile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flyc-io/flyc/control/internal/dataplane"
	"github.com/flyc-io/flyc/control/internal/queueclient"
)

// Desired : état voulu sur les LB.
type Desired struct {
	Backends    []BackendSpec
	Rooms       []dataplane.MapEntry // fqdn → tenant.room
	Backend     []dataplane.MapEntry // fqdn → bk_<tenant>_<name>
	States      []dataplane.MapEntry // tenant.room → state
	Certs       []CertSpec
	PassPub     string // clé publique active (PEM)
	PassPrevPub string // clé précédente encore acceptée (PEM), sinon copie de l'active
	Hash        string
	Queue       map[string]queueclient.RoomConfig // tenant.room → config flyc-queue
}

type BackendSpec struct {
	Name      string
	Balance   string
	CheckPath string
	Servers   []dataplane.Server
}

type CertSpec struct {
	FQDN string
	Hash string // sha256 du PEM
	PEM  []byte
}

// BackendName : nom HAProxy d'un backend client.
func BackendName(tenant, name string) string {
	return "bk_" + strings.ReplaceAll(tenant, "-", "_") + "__" + strings.ReplaceAll(name, "-", "_")
}

// Build lit tout et calcule l'état désiré. decrypt déchiffre les PEM des certificats.
func Build(ctx context.Context, pool *pgxpool.Pool, decrypt func([]byte) ([]byte, error)) (*Desired, error) {
	d := &Desired{Queue: map[string]queueclient.RoomConfig{}}

	// Backends + serveurs
	rows, err := pool.Query(ctx, `select b.id, t.slug, b.name, b.balance, b.check_path from backends b join tenants t on t.id=b.tenant_id order by t.slug, b.name`)
	if err != nil {
		return nil, err
	}
	type bk struct {
		id   string
		spec BackendSpec
	}
	var bks []bk
	for rows.Next() {
		var id, tslug, name, balance, check string
		if err := rows.Scan(&id, &tslug, &name, &balance, &check); err != nil {
			rows.Close()
			return nil, err
		}
		bks = append(bks, bk{id: id, spec: BackendSpec{Name: BackendName(tslug, name), Balance: balance, CheckPath: check}})
	}
	rows.Close()
	for i := range bks {
		srows, err := pool.Query(ctx, `select name, address, port, weight, maxconn from backend_servers where backend_id=$1 order by name`, bks[i].id)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var s dataplane.Server
			var w, mc int64
			if err := srows.Scan(&s.Name, &s.Address, &s.Port, &w, &mc); err != nil {
				srows.Close()
				return nil, err
			}
			s.Weight, s.Check = &w, "enabled"
			if mc > 0 {
				s.Maxconn = &mc
			}
			bks[i].spec.Servers = append(bks[i].spec.Servers, s)
		}
		srows.Close()
		d.Backends = append(d.Backends, bks[i].spec)
	}

	// Rooms → maps + config queue
	rrows, err := pool.Query(ctx, `
		select t.slug, r.slug, r.title, r.effective_state, r.rate, r.max_active, r.wave_s, r.pass_ttl_s, r.opens_at,
		       dm.fqdn, bt.slug, b.name, r.theme, coalesce(dm.mode,'dns')
		from rooms r
		join tenants t on t.id = r.tenant_id
		left join domains dm on dm.id = r.domain_id
		left join backends b on b.id = r.backend_id
		left join tenants bt on bt.id = b.tenant_id
		order by t.slug, r.slug`)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var tslug, rslug, title, state string
		var rate float64
		var maxActive int64
		var waveS, passTTL int
		var opensAt *time.Time
		var fqdn, btslug, bname *string
		var theme map[string]string
		var dmode string
		if err := rrows.Scan(&tslug, &rslug, &title, &state, &rate, &maxActive, &waveS, &passTTL, &opensAt, &fqdn, &btslug, &bname, &theme, &dmode); err != nil {
			rrows.Close()
			return nil, err
		}
		roomID := tslug + "." + rslug
		qc := queueclient.RoomConfig{State: state, Rate: rate, MaxActive: maxActive, WaveSec: int64(waveS), PassTTLS: int64(passTTL), Title: title, Theme: theme}
		if opensAt != nil {
			qc.OpensAt = opensAt.Unix()
		}
		if fqdn != nil {
			qc.Domains = []string{*fqdn}
			if dmode == "dns" { // en mode iframe, le LB ne voit pas le domaine : seul flyc-queue le connaît
				d.Rooms = append(d.Rooms, dataplane.MapEntry{Key: *fqdn, Value: roomID})
				d.States = append(d.States, dataplane.MapEntry{Key: roomID, Value: state})
				if bname != nil && btslug != nil {
					d.Backend = append(d.Backend, dataplane.MapEntry{Key: *fqdn, Value: BackendName(*btslug, *bname)})
				}
			}
		}
		d.Queue[roomID] = qc
	}
	rrows.Close()

	// Certificats émis
	crows, err := pool.Query(ctx, `select fqdn, pem_enc from certificates where status='issued' and pem_enc is not null order by fqdn`)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var fqdn string
		var enc []byte
		if err := crows.Scan(&fqdn, &enc); err != nil {
			crows.Close()
			return nil, err
		}
		pem, err := decrypt(enc)
		if err != nil {
			continue
		}
		h := sha256.Sum256(pem)
		d.Certs = append(d.Certs, CertSpec{FQDN: fqdn, Hash: hex.EncodeToString(h[:]), PEM: pem})
	}
	crows.Close()

	// Clés de signature : l'active, et la précédente si retirée depuis moins de 24 h (rotation).
	krows, err := pool.Query(ctx, `select public_pem, active from signing_keys where active or retired_at > now() - interval '24 hours' order by active desc, created_at desc limit 2`)
	if err != nil {
		return nil, err
	}
	for krows.Next() {
		var pem string
		var active bool
		if krows.Scan(&pem, &active) == nil {
			if active && d.PassPub == "" {
				d.PassPub = pem
			} else if !active && d.PassPrevPub == "" {
				d.PassPrevPub = pem
			}
		}
	}
	krows.Close()
	if d.PassPrevPub == "" {
		d.PassPrevPub = d.PassPub
	}

	sortEntries(d.Rooms)
	sortEntries(d.Backend)
	sortEntries(d.States)
	d.Hash = hashOf(d)
	return d, nil
}

func sortEntries(e []dataplane.MapEntry) {
	sort.Slice(e, func(i, j int) bool { return e[i].Key < e[j].Key })
}

func hashOf(d *Desired) string {
	type light struct {
		B       []BackendSpec
		R, K, S []dataplane.MapEntry
		C       []string
	}
	l := light{B: d.Backends, R: d.Rooms, K: d.Backend, S: d.States}
	for _, c := range d.Certs {
		l.C = append(l.C, c.FQDN+":"+c.Hash)
	}
	l.C = append(l.C, "key:"+d.PassPub, "prev:"+d.PassPrevPub)
	b, _ := json.Marshal(l)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
