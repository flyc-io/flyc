// Package dataplane : client minimal de la HAProxy Dataplane API v3.
package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Base string // http://<mgmt_ip>:5555
	User string
	Pass string
	HTTP *http.Client
}

func New(base, user, pass string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), User: user, Pass: pass, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// APIError porte le code HTTP pour permettre les cas 404/409.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("dataplane %d: %s", e.Status, e.Body) }

func IsStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.Base + "/v3" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	ct := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rdr = bytes.NewReader(b)
		ct = "text/plain"
	case string:
		rdr = strings.NewReader(b)
		ct = "text/plain"
	default:
		bb, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(bb)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Pass)
	if rdr != nil {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) upload(ctx context.Context, method, path, filename string, content []byte, q url.Values, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file_upload", filename)
	_, _ = fw.Write(content)
	_ = mw.Close()
	u := c.Base + "/v3" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, &buf)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Pass)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- infos / version ----

type Info struct {
	API struct {
		Version string `json:"version"`
	} `json:"api"`
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, "GET", "/info", nil, nil, &i)
}

func (c *Client) Version(ctx context.Context) (int64, error) {
	var v int64
	return v, c.do(ctx, "GET", "/services/haproxy/configuration/version", nil, nil, &v)
}

// ---- transactions ----

type Transaction struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int64  `json:"_version"`
}

func (c *Client) BeginTx(ctx context.Context) (*Transaction, error) {
	v, err := c.Version(ctx)
	if err != nil {
		return nil, err
	}
	var t Transaction
	return &t, c.do(ctx, "POST", "/services/haproxy/transactions", url.Values{"version": {fmt.Sprint(v)}}, nil, &t)
}

func (c *Client) CommitTx(ctx context.Context, id string) error {
	return c.do(ctx, "PUT", "/services/haproxy/transactions/"+id, nil, nil, nil)
}

func (c *Client) AbortTx(ctx context.Context, id string) {
	_ = c.do(ctx, "DELETE", "/services/haproxy/transactions/"+id, nil, nil, nil)
}

func tx(id string) url.Values { return url.Values{"transaction_id": {id}} }

// ---- backends & servers ----

type Backend struct {
	Name          string         `json:"name"`
	Mode          string         `json:"mode,omitempty"`
	Balance       *Balance       `json:"balance,omitempty"`
	AdvCheck      string         `json:"adv_check,omitempty"`
	HTTPChkParams *HTTPChkParams `json:"httpchk_params,omitempty"`
	DefaultServer *DefaultServer `json:"default_server,omitempty"`
	From          string         `json:"from,omitempty"`
}
type Balance struct {
	Algorithm string `json:"algorithm"`
}
type HTTPChkParams struct {
	Method string `json:"method,omitempty"`
	URI    string `json:"uri,omitempty"`
}
type DefaultServer struct {
	Check string `json:"check,omitempty"`
	Inter int64  `json:"inter,omitempty"`
	Rise  int64  `json:"rise,omitempty"`
	Fall  int64  `json:"fall,omitempty"`
}
type Server struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Port    int64  `json:"port"`
	Weight  *int64 `json:"weight,omitempty"`
	Maxconn *int64 `json:"maxconn,omitempty"`
	Check   string `json:"check,omitempty"`
}

func (c *Client) Backends(ctx context.Context) ([]Backend, error) {
	var out []Backend
	return out, c.do(ctx, "GET", "/services/haproxy/configuration/backends", nil, nil, &out)
}

func (c *Client) CreateBackend(ctx context.Context, txID string, b Backend) error {
	return c.do(ctx, "POST", "/services/haproxy/configuration/backends", tx(txID), b, nil)
}

func (c *Client) ReplaceBackend(ctx context.Context, txID string, b Backend) error {
	return c.do(ctx, "PUT", "/services/haproxy/configuration/backends/"+b.Name, tx(txID), b, nil)
}

func (c *Client) DeleteBackend(ctx context.Context, txID, name string) error {
	return c.do(ctx, "DELETE", "/services/haproxy/configuration/backends/"+name, tx(txID), nil, nil)
}

func (c *Client) Servers(ctx context.Context, backend string) ([]Server, error) {
	var out []Server
	return out, c.do(ctx, "GET", "/services/haproxy/configuration/backends/"+backend+"/servers", nil, nil, &out)
}

func (c *Client) CreateServer(ctx context.Context, txID, backend string, s Server) error {
	return c.do(ctx, "POST", "/services/haproxy/configuration/backends/"+backend+"/servers", tx(txID), s, nil)
}

func (c *Client) ReplaceServer(ctx context.Context, txID, backend string, s Server) error {
	return c.do(ctx, "PUT", "/services/haproxy/configuration/backends/"+backend+"/servers/"+s.Name, tx(txID), s, nil)
}

func (c *Client) DeleteServer(ctx context.Context, txID, backend, name string) error {
	return c.do(ctx, "DELETE", "/services/haproxy/configuration/backends/"+backend+"/servers/"+name, tx(txID), nil, nil)
}

// ---- maps (runtime, persistées avec force_sync) ----

type MapEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SyncMap amène une map à l'état désiré par différence (ajout / mise à jour / suppression d'entrées),
// à chaud et persisté sur disque. Si la map contient des doublons de clés, elle est vidée puis reconstruite.
func (c *Client) SyncMap(ctx context.Context, name string, desired []MapEntry) error {
	fs := url.Values{"force_sync": {"true"}}
	current, err := c.MapEntries(ctx, name)
	if err != nil && !IsStatus(err, 404) {
		return err
	}
	cur := map[string]string{}
	dup := false
	for _, e := range current {
		if _, seen := cur[e.Key]; seen {
			dup = true
		}
		cur[e.Key] = e.Value
	}
	if dup {
		if err := c.do(ctx, "DELETE", "/services/haproxy/runtime/maps/"+name, url.Values{"force_delete": {"true"}, "force_sync": {"true"}}, nil, nil); err != nil && !IsStatus(err, 404) {
			return fmt.Errorf("vidage de %s: %w", name, err)
		}
		cur = map[string]string{}
	}
	want := map[string]string{}
	for _, e := range desired {
		want[e.Key] = e.Value
	}
	for k, v := range want {
		if cv, ok := cur[k]; !ok {
			if err := c.do(ctx, "POST", "/services/haproxy/runtime/maps/"+name+"/entries", fs, MapEntry{Key: k, Value: v}, nil); err != nil && !IsStatus(err, 409) {
				return fmt.Errorf("%s: ajout %s: %w", name, k, err)
			}
		} else if cv != v {
			if err := c.do(ctx, "PUT", "/services/haproxy/runtime/maps/"+name+"/entries/"+url.PathEscape(k), fs, map[string]string{"value": v}, nil); err != nil {
				return fmt.Errorf("%s: mise à jour %s: %w", name, k, err)
			}
		}
	}
	for k := range cur {
		if _, ok := want[k]; !ok {
			if err := c.do(ctx, "DELETE", "/services/haproxy/runtime/maps/"+name+"/entries/"+url.PathEscape(k), fs, nil, nil); err != nil && !IsStatus(err, 404) {
				return fmt.Errorf("%s: suppression %s: %w", name, k, err)
			}
		}
	}
	return nil
}

func (c *Client) MapEntries(ctx context.Context, name string) ([]MapEntry, error) {
	var out []MapEntry
	return out, c.do(ctx, "GET", "/services/haproxy/runtime/maps/"+name+"/entries", nil, nil, &out)
}

// ---- certificats ----

type StoredCert struct {
	StorageName string `json:"storage_name"`
	File        string `json:"file"`
	NotAfter    string `json:"not_after"`
	Subject     string `json:"subject"`
}

func (c *Client) StoredCerts(ctx context.Context) ([]StoredCert, error) {
	var out []StoredCert
	return out, c.do(ctx, "GET", "/services/haproxy/storage/ssl_certificates", nil, nil, &out)
}

// UpsertCert écrit <name> dans le répertoire SSL du LB. Un remplacement (renouvellement) déclenche
// un reload par la Dataplane API, car HAProxy garde l'ancien certificat en mémoire ; une création
// est suivie d'une mise à jour de crt-list qui recharge elle-même.
func (c *Client) UpsertCert(ctx context.Context, name string, pem []byte) (created bool, err error) {
	err = c.do(ctx, "PUT", "/services/haproxy/storage/ssl_certificates/"+name, nil, string(pem), nil)
	if err == nil {
		return false, nil
	}
	if !IsStatus(err, 404) {
		return false, err
	}
	return true, c.upload(ctx, "POST", "/services/haproxy/storage/ssl_certificates", name, pem, nil, nil)
}

// StorageName : nom de fichier tel que la Dataplane API le normalise (points → underscores).
func StorageName(fqdn string) string {
	return strings.ReplaceAll(strings.TrimSuffix(fqdn, ".pem"), ".", "_") + ".pem"
}

type CrtListEntry struct {
	File          string   `json:"file"`
	SNIFilter     []string `json:"sni_filter,omitempty"`
	SSLBindConfig string   `json:"ssl_bind_config,omitempty"`
	LineNumber    int64    `json:"line_number,omitempty"`
}

// CrtListGet lit le contenu brut d'une crt-list (fichier).
func (c *Client) CrtListGet(ctx context.Context, name string) (string, error) {
	u := c.Base + "/v3/services/haproxy/storage/ssl_crt_lists/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.User, c.Pass)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 404 {
		return "", nil
	}
	if resp.StatusCode >= 300 {
		return "", &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	return string(data), nil
}

// CrtListSync remplace le fichier crt-list par le contenu voulu si différent ; la Dataplane API
// recharge alors HAProxy. Renvoie true si le fichier a changé.
func (c *Client) CrtListSync(ctx context.Context, name, content string) (bool, error) {
	cur, err := c.CrtListGet(ctx, name)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(cur) == strings.TrimSpace(content) {
		return false, nil
	}
	if err := c.do(ctx, "PUT", "/services/haproxy/storage/ssl_crt_lists/"+url.PathEscape(name), nil, content, nil); err != nil {
		return false, err
	}
	return true, nil
}

func crtListBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Reload déclenche un reload (via une transaction vide).
func (c *Client) Reload(ctx context.Context) error {
	t, err := c.BeginTx(ctx)
	if err != nil {
		return err
	}
	return c.CommitTx(ctx, t.ID)
}

// BackendSessions : sessions courantes d'un backend (stats natives, ligne BACKEND).
// La réponse est un objet {runtimeAPI, stats:[…]} ou, selon les versions, une liste de tels objets.
func (c *Client) BackendSessions(ctx context.Context, backend string) (int64, error) {
	type statsObj struct {
		Stats []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Stats struct {
				Scur int64 `json:"scur"`
			} `json:"stats"`
		} `json:"stats"`
	}
	var raw json.RawMessage
	if err := c.do(ctx, "GET", "/services/haproxy/stats/native", url.Values{"type": {"backend"}, "name": {backend}}, nil, &raw); err != nil {
		return 0, err
	}
	var objs []statsObj
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &objs); err != nil {
			return 0, err
		}
	} else {
		var o statsObj
		if err := json.Unmarshal(raw, &o); err != nil {
			return 0, err
		}
		objs = []statsObj{o}
	}
	var total int64
	for _, o := range objs {
		for _, st := range o.Stats {
			if st.Type == "backend" && st.Name == backend {
				total += st.Stats.Scur
			}
		}
	}
	return total, nil
}

// GeneralSync écrit un fichier du stockage général (/etc/haproxy/general) s'il diffère. Renvoie true si changé.
// Création et remplacement passent tous deux par un envoi multipart (seul format accepté par l'API).
func (c *Client) GeneralSync(ctx context.Context, name, content string) (bool, error) {
	u := c.Base + "/v3/services/haproxy/storage/general/" + url.PathEscape(name)
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.SetBasicAuth(c.User, c.Pass)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 && strings.TrimSpace(string(data)) == strings.TrimSpace(content) {
		return false, nil
	}
	if resp.StatusCode == 404 {
		return true, c.upload(ctx, "POST", "/services/haproxy/storage/general", name, []byte(content), nil, nil)
	}
	return true, c.upload(ctx, "PUT", "/services/haproxy/storage/general/"+url.PathEscape(name), name, []byte(content), url.Values{"skip_reload": {"true"}}, nil)
}
