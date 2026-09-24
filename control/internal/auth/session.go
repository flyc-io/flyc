package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	CookieName  = "flyc_session"
	SessionTTL  = 12 * time.Hour
	sessionSlid = 30 * time.Minute // prolongation quand il reste moins que ça
)

// User connecté.
type User struct {
	ID                 string
	Email              string
	DisplayName        string
	PlatformAdmin      bool
	TOTPEnabled        bool
	MustChangePassword bool
}

// Sessions : création, lecture, révocation.
type Sessions struct {
	Pool   *pgxpool.Pool
	Secure bool // cookie Secure (false seulement en HTTP de test)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// Create ouvre une session et pose le cookie.
func (s *Sessions) Create(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) error {
	tok := RandomToken()
	ip := r.Header.Get("X-Real-IP")
	if ip == "" {
		ip = r.RemoteAddr
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if _, err := s.Pool.Exec(ctx, `insert into sessions(id, user_id, expires_at, ip, user_agent) values($1,$2,$3,$4,$5)`,
		hashToken(tok), userID, time.Now().Add(SessionTTL), ip, ua); err != nil {
		return err
	}
	_, _ = s.Pool.Exec(ctx, `update users set last_login_at=now() where id=$1`, userID)
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: int(SessionTTL.Seconds())})
	return nil
}

// Current renvoie l'utilisateur de la session du cookie, ou nil.
func (s *Sessions) Current(ctx context.Context, w http.ResponseWriter, r *http.Request) (*User, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	var u User
	var exp time.Time
	err = s.Pool.QueryRow(ctx, `select u.id, u.email, u.display_name, u.is_platform_admin, u.totp_enabled, u.must_change_password, s.expires_at
		from sessions s join users u on u.id = s.user_id where s.id=$1 and s.expires_at > now()`, hashToken(c.Value)).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.PlatformAdmin, &u.TOTPEnabled, &u.MustChangePassword, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if time.Until(exp) < SessionTTL-sessionSlid {
		_, _ = s.Pool.Exec(ctx, `update sessions set expires_at=$2 where id=$1`, hashToken(c.Value), time.Now().Add(SessionTTL))
	}
	return &u, nil
}

// Destroy révoque la session courante et efface le cookie.
func (s *Sessions) Destroy(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		_, _ = s.Pool.Exec(ctx, `delete from sessions where id=$1`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

// DestroyAll révoque toutes les sessions d'un utilisateur (changement de mot de passe).
func (s *Sessions) DestroyAll(ctx context.Context, userID string) {
	_, _ = s.Pool.Exec(ctx, `delete from sessions where user_id=$1`, userID)
}

// Purge : sessions expirées et états OIDC anciens.
func (s *Sessions) Purge(ctx context.Context) {
	_, _ = s.Pool.Exec(ctx, `delete from sessions where expires_at < now()`)
	_, _ = s.Pool.Exec(ctx, `delete from oidc_states where created_at < now() - interval '15 minutes'`)
}
