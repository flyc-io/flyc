package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flyc-io/flyc/control/internal/auth"
	"github.com/flyc-io/flyc/control/internal/bootstrap"
	"github.com/flyc-io/flyc/internal/httpx"
)

// authenticateSession : principal issu du cookie de session.
// Les requêtes non-GET exigent l'en-tête X-Requested-With (protection CSRF, en plus de SameSite=Strict).
func (a *API) authenticateSession(r *http.Request) (*principal, error) {
	u, err := a.sessions.Current(r.Context(), nil, r)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errors.New("non connecté")
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "flyc" {
		return nil, errors.New("en-tête X-Requested-With requis")
	}
	p := &principal{user: u, session: true, platform: u.PlatformAdmin}
	slug := r.Header.Get("X-Flyc-Tenant")
	if u.PlatformAdmin {
		p.role = "owner"
		if slug != "" {
			if err := a.pool.QueryRow(r.Context(), `select id, slug from tenants where slug=$1`, slug).Scan(&p.tenantID, &p.tenant); err != nil {
				return nil, errors.New("tenant inconnu")
			}
		}
		return p, nil
	}
	// utilisateur de tenant : le tenant demandé doit figurer dans ses appartenances (sinon la première)
	q := `select t.id, t.slug, m.role from memberships m join tenants t on t.id=m.tenant_id where m.user_id=$1`
	args := []any{u.ID}
	if slug != "" {
		q += ` and t.slug=$2`
		args = append(args, slug)
	}
	q += ` order by t.slug limit 1`
	err = a.pool.QueryRow(r.Context(), q, args...).Scan(&p.tenantID, &p.tenant, &p.role)
	if errors.Is(err, pgx.ErrNoRows) {
		if slug != "" {
			return nil, errors.New("vous n'êtes pas membre de ce tenant")
		}
		return p, nil // connecté mais sans tenant : seul /v1/account est utile
	}
	return p, err
}

// registerAuth : connexion, déconnexion, OIDC, compte.
func (a *API) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/config", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]any{"oidc": a.oidc.Enabled, "oidc_issuer": a.oidc.Issuer, "wait_host": os.Getenv("FLYC_WAIT_HOST")})
	})
	mux.HandleFunc("POST /auth/login", a.login)
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		a.sessions.Destroy(r.Context(), w, r)
		httpx.JSON(w, 200, map[string]string{"status": "logged_out"})
	})
	mux.HandleFunc("GET /auth/oidc/start", a.oidcStart)
	mux.HandleFunc("GET /auth/oidc/callback", a.oidcCallback)

	mux.HandleFunc("GET /v1/account", a.session(a.account))
	mux.HandleFunc("POST /v1/account/password", a.session(a.changePassword))
	mux.HandleFunc("POST /v1/account/totp/setup", a.session(a.totpSetup))
	mux.HandleFunc("GET /v1/account/totp/qr.png", a.session(a.totpQR))
	mux.HandleFunc("POST /v1/account/totp/enable", a.session(a.totpEnable))
	mux.HandleFunc("POST /v1/account/totp/disable", a.session(a.totpDisable))

	mux.HandleFunc("GET /v1/users", a.platform(a.listUsers))
	mux.HandleFunc("POST /v1/users", a.platform(a.createUser))
	mux.HandleFunc("DELETE /v1/users/{id}", a.platform(a.deleteUser))
	mux.HandleFunc("POST /v1/users/{id}/password", a.platform(a.resetUserPassword))
	mux.HandleFunc("GET /v1/tenants/{slug}/members", a.platform(a.listMembers))
	mux.HandleFunc("PUT /v1/tenants/{slug}/members", a.platform(a.putMember))
	mux.HandleFunc("DELETE /v1/tenants/{slug}/members/{email}", a.platform(a.deleteMember))
	mux.HandleFunc("GET /v1/tenants/{slug}/api-keys", a.platform(a.listTenantKeysPlatform))
	mux.HandleFunc("GET /v1/keys", a.platform(a.listSigningKeys))
	mux.HandleFunc("POST /v1/keys/rotate", a.platform(a.rotateSigningKey))
}

// ---- connexion locale ----

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	var id, hash string
	var totpEnabled, mustChange bool
	var totpEnc []byte
	err := a.pool.QueryRow(r.Context(), `select id, coalesce(password_hash,''), totp_enabled, totp_secret, must_change_password from users where lower(email)=$1`, in.Email).
		Scan(&id, &hash, &totpEnabled, &totpEnc, &mustChange)
	// délai constant approximatif : on vérifie toujours un hachage
	if err != nil || hash == "" {
		auth.VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", in.Password)
		httpx.Error(w, 401, "bad_credentials", "email ou mot de passe incorrect")
		return
	}
	if !auth.VerifyPassword(hash, in.Password) {
		httpx.Error(w, 401, "bad_credentials", "email ou mot de passe incorrect")
		return
	}
	if totpEnabled {
		if in.TOTP == "" {
			httpx.JSON(w, 428, map[string]any{"error": "totp_required", "message": "code de validation requis"})
			return
		}
		secret, err := a.key.Open(totpEnc)
		if err != nil || !auth.TOTPValidate(string(secret), strings.TrimSpace(in.TOTP)) {
			httpx.Error(w, 401, "bad_totp", "code de validation incorrect")
			return
		}
	}
	if err := a.sessions.Create(r.Context(), w, r, id); err != nil {
		httpx.Error(w, 500, "session", err.Error())
		return
	}
	a.audit(r.Context(), &principal{}, "auth.login", map[string]string{"email": in.Email})
	httpx.JSON(w, 200, map[string]any{"status": "ok", "must_change_password": mustChange})
}

// ---- OIDC ----

func (a *API) oidcStart(w http.ResponseWriter, r *http.Request) {
	if !a.oidc.Enabled {
		httpx.Error(w, 404, "oidc_disabled", "OIDC n'est pas configuré")
		return
	}
	state, nonce := auth.RandomToken(), auth.RandomToken()
	redirect := r.URL.Query().Get("redirect")
	if !strings.HasPrefix(redirect, "/") {
		redirect = "/"
	}
	if _, err := a.pool.Exec(r.Context(), `insert into oidc_states(state, nonce, redirect) values($1,$2,$3)`, state, nonce, redirect); err != nil {
		httpx.Error(w, 500, "db", err.Error())
		return
	}
	http.Redirect(w, r, a.oidc.AuthURL(state, nonce), http.StatusFound)
}

func (a *API) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if !a.oidc.Enabled {
		httpx.Error(w, 404, "oidc_disabled", "OIDC n'est pas configuré")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	var nonce, redirect string
	if err := a.pool.QueryRow(r.Context(), `delete from oidc_states where state=$1 and created_at > now() - interval '15 minutes' returning nonce, redirect`, state).Scan(&nonce, &redirect); err != nil {
		http.Redirect(w, r, "/#/login?error="+url.QueryEscape("état OIDC invalide ou expiré"), http.StatusFound)
		return
	}
	claims, err := a.oidc.Exchange(r.Context(), code, nonce)
	if err != nil {
		a.log.Warn("oidc", "err", err)
		http.Redirect(w, r, "/#/login?error="+url.QueryEscape("connexion OIDC refusée"), http.StatusFound)
		return
	}
	var id string
	err = a.pool.QueryRow(r.Context(), `select id from users where oidc_subject=$1 or lower(email)=$2`, claims.Subject, claims.Email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := a.pool.QueryRow(r.Context(), `insert into users(email, oidc_subject, display_name, is_platform_admin) values($1,$2,$3,$4) returning id`,
			claims.Email, claims.Subject, claims.Name, a.oidc.AdminEmails[claims.Email]).Scan(&id); err != nil {
			httpx.Error(w, 500, "db", err.Error())
			return
		}
		a.log.Info("utilisateur OIDC créé", "email", claims.Email)
	} else if err != nil {
		httpx.Error(w, 500, "db", err.Error())
		return
	} else {
		_, _ = a.pool.Exec(r.Context(), `update users set oidc_subject=$2, display_name=case when display_name='' then $3 else display_name end where id=$1`, id, claims.Subject, claims.Name)
		if a.oidc.AdminEmails[claims.Email] {
			_, _ = a.pool.Exec(r.Context(), `update users set is_platform_admin=true where id=$1`, id)
		}
	}
	if err := a.sessions.Create(r.Context(), w, r, id); err != nil {
		httpx.Error(w, 500, "session", err.Error())
		return
	}
	a.audit(r.Context(), &principal{}, "auth.oidc", map[string]string{"email": claims.Email})
	http.Redirect(w, r, redirect, http.StatusFound)
}

// ---- compte ----

func (a *API) account(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := a.pool.Query(r.Context(), `select t.slug, t.name, m.role from memberships m join tenants t on t.id=m.tenant_id where m.user_id=$1 order by t.slug`, p.user.ID)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	members := []map[string]string{}
	for rows.Next() {
		var slug, name, role string
		_ = rows.Scan(&slug, &name, &role)
		members = append(members, map[string]string{"tenant": slug, "name": name, "role": role})
	}
	httpx.JSON(w, 200, map[string]any{"email": p.user.Email, "display_name": p.user.DisplayName, "platform_admin": p.user.PlatformAdmin,
		"totp_enabled": p.user.TOTPEnabled, "must_change_password": p.user.MustChangePassword, "memberships": members})
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	var hash string
	_ = a.pool.QueryRow(r.Context(), `select coalesce(password_hash,'') from users where id=$1`, p.user.ID).Scan(&hash)
	if hash != "" && !auth.VerifyPassword(hash, in.Current) {
		httpx.Error(w, 401, "bad_credentials", "mot de passe actuel incorrect")
		return
	}
	if err := auth.CheckPasswordPolicy(in.New); err != nil {
		httpx.Error(w, 400, "weak_password", err.Error())
		return
	}
	if _, err := a.pool.Exec(r.Context(), `update users set password_hash=$2, must_change_password=false where id=$1`, p.user.ID, auth.HashPassword(in.New)); err != nil {
		dbErr(w, err)
		return
	}
	a.sessions.DestroyAll(r.Context(), p.user.ID)
	_ = a.sessions.Create(r.Context(), w, r, p.user.ID)
	a.audit(r.Context(), p, "account.password", nil)
	httpx.JSON(w, 200, map[string]string{"status": "changed"})
}

func (a *API) totpSetup(w http.ResponseWriter, r *http.Request, p *principal) {
	secret, otpURL, err := auth.TOTPGenerate(a.issuer, p.user.Email)
	if err != nil {
		httpx.Error(w, 500, "totp", err.Error())
		return
	}
	enc, err := a.key.Seal([]byte(secret))
	if err != nil {
		httpx.Error(w, 500, "crypt", err.Error())
		return
	}
	if _, err := a.pool.Exec(r.Context(), `update users set totp_secret=$2, totp_enabled=false where id=$1`, p.user.ID, enc); err != nil {
		dbErr(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"secret": secret, "otpauth_url": otpURL})
}

func (a *API) totpQR(w http.ResponseWriter, r *http.Request, p *principal) {
	var enc []byte
	if err := a.pool.QueryRow(r.Context(), `select totp_secret from users where id=$1`, p.user.ID).Scan(&enc); err != nil || len(enc) == 0 {
		httpx.Error(w, 404, "no_totp", "lancez d'abord la configuration")
		return
	}
	secret, err := a.key.Open(enc)
	if err != nil {
		httpx.Error(w, 500, "crypt", err.Error())
		return
	}
	otpURL := "otpauth://totp/" + url.PathEscape(a.issuer+":"+p.user.Email) + "?secret=" + string(secret) + "&issuer=" + url.QueryEscape(a.issuer)
	png, err := auth.TOTPQRPNG(otpURL)
	if err != nil {
		httpx.Error(w, 500, "qr", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (a *API) totpEnable(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	var enc []byte
	if err := a.pool.QueryRow(r.Context(), `select totp_secret from users where id=$1`, p.user.ID).Scan(&enc); err != nil || len(enc) == 0 {
		httpx.Error(w, 404, "no_totp", "lancez d'abord la configuration")
		return
	}
	secret, err := a.key.Open(enc)
	if err != nil || !auth.TOTPValidate(string(secret), strings.TrimSpace(in.Code)) {
		httpx.Error(w, 400, "bad_totp", "code incorrect")
		return
	}
	_, _ = a.pool.Exec(r.Context(), `update users set totp_enabled=true where id=$1`, p.user.ID)
	a.audit(r.Context(), p, "account.totp_enabled", nil)
	httpx.JSON(w, 200, map[string]string{"status": "enabled"})
}

func (a *API) totpDisable(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	var hash string
	_ = a.pool.QueryRow(r.Context(), `select coalesce(password_hash,'') from users where id=$1`, p.user.ID).Scan(&hash)
	if hash != "" && !auth.VerifyPassword(hash, in.Password) {
		httpx.Error(w, 401, "bad_credentials", "mot de passe incorrect")
		return
	}
	_, _ = a.pool.Exec(r.Context(), `update users set totp_enabled=false, totp_secret=null where id=$1`, p.user.ID)
	a.audit(r.Context(), p, "account.totp_disabled", nil)
	httpx.JSON(w, 200, map[string]string{"status": "disabled"})
}

// ---- utilisateurs et appartenances (plateforme) ----

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select u.id, u.email, u.display_name, u.is_platform_admin, u.totp_enabled, u.oidc_subject is not null, u.created_at, u.last_login_at,
		coalesce((select string_agg(t.slug||':'||m.role, ',' order by t.slug) from memberships m join tenants t on t.id=m.tenant_id where m.user_id=u.id),'')
		from users u order by u.email`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, email, name, members string
		var admin, totp, oidc bool
		var created time.Time
		var last *time.Time
		_ = rows.Scan(&id, &email, &name, &admin, &totp, &oidc, &created, &last, &members)
		out = append(out, map[string]any{"id": id, "email": email, "display_name": name, "platform_admin": admin, "totp_enabled": totp, "oidc": oidc, "created_at": created, "last_login_at": last, "memberships": members})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Email         string `json:"email"`
		Password      string `json:"password"`
		DisplayName   string `json:"display_name"`
		PlatformAdmin bool   `json:"platform_admin"`
		Tenant        string `json:"tenant"`
		Role          string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(in.Email, "@") {
		httpx.Error(w, 400, "invalid", "email invalide")
		return
	}
	if in.Password == "" {
		in.Password = auth.RandomToken()[:16]
	}
	if err := auth.CheckPasswordPolicy(in.Password); err != nil {
		httpx.Error(w, 400, "weak_password", err.Error())
		return
	}
	var id string
	if err := a.pool.QueryRow(r.Context(), `insert into users(email, password_hash, display_name, is_platform_admin, must_change_password) values($1,$2,$3,$4,true) returning id`,
		in.Email, auth.HashPassword(in.Password), in.DisplayName, in.PlatformAdmin).Scan(&id); err != nil {
		dbErr(w, err)
		return
	}
	if in.Tenant != "" {
		if in.Role == "" {
			in.Role = "operator"
		}
		if _, err := a.pool.Exec(r.Context(), `insert into memberships(tenant_id, user_id, role) select id, $2, $3 from tenants where slug=$1`, in.Tenant, id, in.Role); err != nil {
			dbErr(w, err)
			return
		}
	}
	a.audit(r.Context(), p, "user.create", map[string]any{"email": in.Email, "platform_admin": in.PlatformAdmin, "tenant": in.Tenant, "role": in.Role})
	httpx.JSON(w, 201, map[string]any{"id": id, "email": in.Email, "initial_password": in.Password})
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request, p *principal) {
	if p.user != nil && p.user.ID == r.PathValue("id") {
		httpx.Error(w, 400, "self", "impossible de supprimer son propre compte")
		return
	}
	tag, err := a.pool.Exec(r.Context(), `delete from users where id=$1`, r.PathValue("id"))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "utilisateur inconnu")
		return
	}
	a.audit(r.Context(), p, "user.delete", r.PathValue("id"))
	w.WriteHeader(204)
}

func (a *API) resetUserPassword(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Password == "" {
		in.Password = auth.RandomToken()[:16]
	}
	if err := auth.CheckPasswordPolicy(in.Password); err != nil {
		httpx.Error(w, 400, "weak_password", err.Error())
		return
	}
	tag, err := a.pool.Exec(r.Context(), `update users set password_hash=$2, must_change_password=true, totp_enabled=false, totp_secret=null where id=$1`, r.PathValue("id"), auth.HashPassword(in.Password))
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "utilisateur inconnu")
		return
	}
	a.sessions.DestroyAll(r.Context(), r.PathValue("id"))
	a.audit(r.Context(), p, "user.password_reset", r.PathValue("id"))
	httpx.JSON(w, 200, map[string]string{"initial_password": in.Password})
}

func (a *API) listMembers(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select u.email, u.display_name, m.role from memberships m join users u on u.id=m.user_id join tenants t on t.id=m.tenant_id where t.slug=$1 order by u.email`, r.PathValue("slug"))
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var e, n, role string
		_ = rows.Scan(&e, &n, &role)
		out = append(out, map[string]string{"email": e, "display_name": n, "role": role})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) putMember(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	if roleRank[in.Role] == 0 {
		httpx.Error(w, 400, "invalid", "role : owner, admin, operator ou viewer")
		return
	}
	tag, err := a.pool.Exec(r.Context(), `insert into memberships(tenant_id, user_id, role)
		select t.id, u.id, $3 from tenants t, users u where t.slug=$1 and lower(u.email)=$2
		on conflict (tenant_id, user_id) do update set role=excluded.role`, r.PathValue("slug"), strings.ToLower(in.Email), in.Role)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "tenant ou utilisateur inconnu")
		return
	}
	a.audit(r.Context(), p, "member.put", map[string]string{"tenant": r.PathValue("slug"), "email": in.Email, "role": in.Role})
	httpx.JSON(w, 200, in)
}

func (a *API) deleteMember(w http.ResponseWriter, r *http.Request, p *principal) {
	_, err := a.pool.Exec(r.Context(), `delete from memberships m using tenants t, users u where m.tenant_id=t.id and m.user_id=u.id and t.slug=$1 and lower(u.email)=$2`, r.PathValue("slug"), strings.ToLower(r.PathValue("email")))
	if err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "member.delete", map[string]string{"tenant": r.PathValue("slug"), "email": r.PathValue("email")})
	w.WriteHeader(204)
}

// ---- clés API d'un tenant ----

func (a *API) listTenantKeys(w http.ResponseWriter, r *http.Request, p *principal) {
	a.listKeysFor(w, r, p.tenantID)
}

func (a *API) listTenantKeysPlatform(w http.ResponseWriter, r *http.Request, _ *principal) {
	var id string
	if err := a.pool.QueryRow(r.Context(), `select id from tenants where slug=$1`, r.PathValue("slug")).Scan(&id); err != nil {
		dbErr(w, err)
		return
	}
	a.listKeysFor(w, r, id)
}

func (a *API) listKeysFor(w http.ResponseWriter, r *http.Request, tenantID string) {
	rows, err := a.pool.Query(r.Context(), `select id, name, created_at, last_used_at from api_keys where tenant_id=$1 order by created_at`, tenantID)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		var created time.Time
		var used *time.Time
		_ = rows.Scan(&id, &name, &created, &used)
		out = append(out, map[string]any{"id": id, "name": name, "created_at": created, "last_used_at": used})
	}
	httpx.JSON(w, 200, out)
}

func (a *API) createOwnTenantKey(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Name == "" {
		in.Name = "key-" + time.Now().Format("20060102-150405")
	}
	key := NewKey()
	if _, err := a.pool.Exec(r.Context(), `insert into api_keys(tenant_id, name, key_hash) values($1,$2,$3)`, p.tenantID, in.Name, HashKey(key)); err != nil {
		dbErr(w, err)
		return
	}
	a.audit(r.Context(), p, "apikey.create", map[string]string{"name": in.Name})
	httpx.JSON(w, 201, map[string]string{"name": in.Name, "api_key": key})
}

func (a *API) deleteTenantKey(w http.ResponseWriter, r *http.Request, p *principal) {
	tag, err := a.pool.Exec(r.Context(), `delete from api_keys where id=$1 and tenant_id=$2`, r.PathValue("id"), p.tenantID)
	if err != nil {
		dbErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.Error(w, 404, "not_found", "clé inconnue")
		return
	}
	a.audit(r.Context(), p, "apikey.delete", r.PathValue("id"))
	w.WriteHeader(204)
}

var _ = context.Background

// ---- clés de signature des pass ----

func (a *API) listSigningKeys(w http.ResponseWriter, r *http.Request, _ *principal) {
	rows, err := a.pool.Query(r.Context(), `select kid, active, created_at, retired_at from signing_keys order by created_at desc`)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var kid string
		var active bool
		var created time.Time
		var retired *time.Time
		_ = rows.Scan(&kid, &active, &created, &retired)
		out = append(out, map[string]any{"kid": kid, "active": active, "created_at": created, "retired_at": retired, "accepted": active || (retired != nil && time.Since(*retired) < 24*time.Hour)})
	}
	httpx.JSON(w, 200, out)
}

// rotateSigningKey : nouvelle paire ES256 active ; l'ancienne reste acceptée 24 h (LB, JWKS, verify).
// flyc-queue signe avec la nouvelle clé dès son prochain rafraîchissement (≤ 60 s).
func (a *API) rotateSigningKey(w http.ResponseWriter, r *http.Request, p *principal) {
	res, err := bootstrap.RotateSigningKey(r.Context(), a.pool, a.key)
	if err != nil {
		httpx.Error(w, 500, "rotate", err.Error())
		return
	}
	a.audit(r.Context(), p, "keys.rotate", map[string]string{"kid": res.KID})
	a.onKeyRotated(res)
	a.rec.Kick()
	httpx.JSON(w, 200, map[string]any{"kid": res.KID, "status": "rotated"})
}
