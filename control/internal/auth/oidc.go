package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC : fournisseur externe optionnel (Pocket ID, Keycloak, Authentik…).
type OIDC struct {
	Enabled     bool
	Issuer      string
	ClientID    string
	RedirectURL string
	AdminEmails map[string]bool // emails promus administrateurs plateforme à la connexion
	provider    *gooidc.Provider
	cfg         oauth2.Config
	verifier    *gooidc.IDTokenVerifier
}

// NewOIDC initialise le fournisseur ; désactivé si issuer ou client_id manquent.
func NewOIDC(ctx context.Context, issuer, clientID, clientSecret, redirectURL, adminEmails string, log *slog.Logger) *OIDC {
	o := &OIDC{Issuer: issuer, ClientID: clientID, RedirectURL: redirectURL, AdminEmails: map[string]bool{}}
	for _, e := range strings.Split(adminEmails, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			o.AdminEmails[e] = true
		}
	}
	if issuer == "" || clientID == "" {
		return o
	}
	p, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		log.Warn("OIDC désactivé : fournisseur injoignable", "issuer", issuer, "err", err)
		return o
	}
	o.provider = p
	o.cfg = oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL, Endpoint: p.Endpoint(), Scopes: []string{gooidc.ScopeOpenID, "email", "profile"}}
	o.verifier = p.Verifier(&gooidc.Config{ClientID: clientID})
	o.Enabled = true
	log.Info("OIDC activé", "issuer", issuer)
	return o
}

// AuthURL : URL de redirection vers le fournisseur.
func (o *OIDC) AuthURL(state, nonce string) string {
	return o.cfg.AuthCodeURL(state, gooidc.Nonce(nonce))
}

// Claims utiles renvoyés par le fournisseur.
type Claims struct {
	Subject string
	Email   string
	Name    string
}

// Exchange échange le code et vérifie l'ID token et le nonce.
func (o *OIDC) Exchange(ctx context.Context, code, nonce string) (*Claims, error) {
	tok, err := o.cfg.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("pas d'ID token dans la réponse")
	}
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	if idt.Nonce != nonce {
		return nil, errors.New("nonce invalide")
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Preferred     string `json:"preferred_username"`
	}
	if err := idt.Claims(&c); err != nil {
		return nil, err
	}
	if c.Email == "" {
		return nil, errors.New("le fournisseur n'a pas renvoyé d'email")
	}
	name := c.Name
	if name == "" {
		name = c.Preferred
	}
	return &Claims{Subject: idt.Subject, Email: strings.ToLower(c.Email), Name: name}, nil
}
