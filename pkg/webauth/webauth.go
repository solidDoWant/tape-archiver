// Package webauth adds provider-agnostic OIDC authorization-code-flow
// authentication to cmd/web. Any authenticated user is authorized.
//
// Sessions are an encrypted AES-256-GCM cookie, not a server-side store
// (SPEC §4.2 forbids UI-owned state). Unauthenticated page requests are served
// the SPA, which renders its own login page; unauthenticated /api/* requests
// get a 401 JSON body.
package webauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/tape-archiver/pkg/webserver"
)

const sessionKeyLen = 32

const stateCookieTTL = 10 * time.Minute

// The session lifetime is clamped to [minSessionDuration, maxSessionDuration]
// regardless of the ID token's expiry: there is no refresh flow, and short
// IdP token lifetimes must not bounce an operator mid-task (e.g. across a
// backup's eject-pause wait).
const maxSessionDuration = 24 * time.Hour

const minSessionDuration = 8 * time.Hour

// discoveryTimeout keeps an unreachable issuer from hanging startup while
// /readyz already reports ready.
const discoveryTimeout = 10 * time.Second

const (
	sessionCookieName = "ta_session"
	stateCookieName   = "ta_oidc_state"
)

// AAD purposes, so one cookie kind can never be replayed as the other.
const (
	sessionPurpose = "session"
	statePurpose   = "state"
)

// Config configures an Authenticator. All fields except AppVersion and
// FooterHost are required.
type Config struct {
	// IssuerURL is used for OIDC discovery.
	IssuerURL string
	// ClientID and ClientSecret identify a confidential client.
	ClientID     string
	ClientSecret string
	// RedirectURL is the /auth/callback URL registered with the provider.
	RedirectURL string
	// SessionKey is the 32-byte AES-256-GCM cookie key (see ParseSessionKey).
	// Rotating it logs everyone out.
	SessionKey []byte
	// AppVersion is served ungated at GET /api/build-info for the UI footer.
	AppVersion string
	// FooterHost is an optional deploy label appended to the footer.
	FooterHost string
}

// Identity is the authenticated user, as returned by GET /api/me.
type Identity struct {
	// Subject is the OIDC "sub" claim.
	Subject string `json:"subject"`
	// Name falls back to the "preferred_username" claim.
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Authenticator holds the OIDC client and session-cookie cipher. Build one
// with New and mount it with Wrap.
type Authenticator struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	gcm          cipher.AEAD
	appVersion   string
	footerHost   string
}

// ParseSessionKey decodes a base64-encoded 32-byte key (e.g. from
// `openssl rand -base64 32`) for Config.SessionKey.
func ParseSessionKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("webauth: session key must be base64-encoded: %w", err)
	}

	if len(key) != sessionKeyLen {
		return nil, fmt.Errorf("webauth: session key must decode to %d bytes (AES-256), got %d", sessionKeyLen, len(key))
	}

	return key, nil
}

// New discovers the OIDC provider at cfg.IssuerURL and builds an
// Authenticator. A cancelled ctx surfaces as ctx.Err(), an orderly stop.
func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(discoveryCtx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("webauth: discover OIDC provider %q: %w", cfg.IssuerURL, err)
	}

	block, err := aes.NewCipher(cfg.SessionKey)
	if err != nil {
		return nil, fmt.Errorf("webauth: build session cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webauth: build session cipher: %w", err)
	}

	return &Authenticator{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier:   provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		gcm:        gcm,
		appVersion: cfg.AppVersion,
		footerHost: cfg.FooterHost,
	}, nil
}

func validateConfig(cfg Config) error {
	switch {
	case cfg.IssuerURL == "":
		return errors.New("webauth: issuer URL is required")
	case cfg.ClientID == "":
		return errors.New("webauth: client ID is required")
	case cfg.ClientSecret == "":
		return errors.New("webauth: client secret is required")
	case cfg.RedirectURL == "":
		return errors.New("webauth: redirect URL is required")
	case len(cfg.SessionKey) != sessionKeyLen:
		return fmt.Errorf("webauth: session key must be %d bytes (AES-256), got %d", sessionKeyLen, len(cfg.SessionKey))
	default:
		return nil
	}
}

// Wrap returns next behind the session gate, plus the /auth/* routes and the
// ungated GET /api/build-info (rendered on the login page) and gated GET
// /api/me.
func (a *Authenticator) Wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/build-info", a.handleBuildInfo)
	mux.Handle("GET /api/me", a.requireSession(http.HandlerFunc(a.handleMe)))
	mux.Handle("/", a.requireSession(next))

	return mux
}

// requireSession attaches the session's Identity to the context. Only
// unauthenticated /api/* requests are rejected (401); pages are served
// regardless, since the SPA carries no secrets and renders the login page.
func (a *Authenticator) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CSRF defence in depth on top of SameSite=Lax, which does not stop a
		// same-site (sibling-subdomain) forgery.
		if strings.HasPrefix(r.URL.Path, "/api/") && isCrossSiteMutation(r) {
			webserver.Annotate(r.Context(), slog.String("deny_reason", "cross-site mutation blocked (CSRF)"))
			writeJSONError(w, http.StatusForbidden, "cross-site request forbidden")

			return
		}

		identity, ok := a.identityFromSessionCookie(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				// The 401 is opaque; only the access log gets the reason.
				webserver.Annotate(r.Context(), slog.String("deny_reason", sessionDenyReason(r)))
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")

				return
			}

			next.ServeHTTP(w, r)

			return
		}

		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
	})
}

func sessionDenyReason(r *http.Request) string {
	if _, err := r.Cookie(sessionCookieName); err != nil {
		return "no session cookie"
	}

	return "invalid or expired session cookie"
}

// isCrossSiteMutation reports whether a browser header positively marks r as
// a cross-site state-changing request. Non-browser clients send neither
// Sec-Fetch-Site nor Origin and are never blocked.
func isCrossSiteMutation(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		// Safe methods; the mutating API is POST-only.
		return false
	}

	// "same-site" is not trusted: a sibling subdomain reports it, so it falls
	// through to the Origin host check.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "cross-site":
		return true
	}

	// Browsers send Origin on every unsafe cross-origin request, so its absence
	// means same-origin or a non-browser client.
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return true
	}

	return !strings.EqualFold(parsed.Host, r.Host)
}

// handleLogin mints state, nonce, and PKCE verifier into the encrypted state
// cookie and redirects to the provider.
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	nonce := randomToken()
	verifier := oauth2.GenerateVerifier()

	claims := stateClaims{
		State:        state,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		RedirectPath: sanitizeRedirectPath(r.URL.Query().Get("redirect")),
		ExpiresAt:    time.Now().Add(stateCookieTTL).Unix(),
	}

	value, err := a.encrypt(statePurpose, claims)
	if err != nil {
		http.Error(w, "failed to start login", http.StatusInternalServerError)

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(stateCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})

	authURL := a.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleCallback completes the flow and sets the session cookie. Failures
// redirect to the SPA login page rather than an HTTP error: "denied" only
// for an explicit IdP denial, "expired" (just retry) for everything else.
func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	state, ok := a.consumeStateCookie(w, r)
	if !ok {
		// No state cookie, so no RedirectPath to preserve.
		rejectCallback(w, r, loginErrorExpired, "", "missing or expired login-state cookie")

		return
	}

	query := r.URL.Query()

	if errParam := query.Get("error"); errParam != "" {
		// The app applies no authorization of its own, so the IdP's error is
		// the only diagnostic (e.g. a provider-side role assignment).
		description := query.Get("error_description")

		webserver.Annotate(r.Context(),
			slog.String("auth_error", "identity provider returned an error"),
			slog.String("idp_error", errParam),
			slog.String("idp_error_description", description))

		loginErrorRedirect(w, r, loginErrorDenied, state.RedirectPath, sanitizeIdPErrorDescription(description))

		return
	}

	returnedState := query.Get("state")
	if returnedState == "" || subtle.ConstantTimeCompare([]byte(returnedState), []byte(state.State)) != 1 {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "state mismatch (stale login attempt or CSRF)")

		return
	}

	code := query.Get("code")
	if code == "" {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "no authorization code in callback")

		return
	}

	token, err := a.oauth2Config.Exchange(r.Context(), code, oauth2.VerifierOption(state.PKCEVerifier))
	if err != nil {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "authorization code exchange failed",
			slog.Any("error", err))

		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "no id_token in token response")

		return
	}

	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "id_token verification failed",
			slog.Any("error", err))

		return
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(state.Nonce)) != 1 {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "id_token nonce mismatch")

		return
	}

	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}

	if err := idToken.Claims(&claims); err != nil {
		rejectCallback(w, r, loginErrorExpired, state.RedirectPath, "id_token claims decode failed",
			slog.Any("error", err))

		return
	}

	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}

	now := time.Now()

	expiresAt := idToken.Expiry
	if minExpiry := now.Add(minSessionDuration); expiresAt.Before(minExpiry) {
		expiresAt = minExpiry
	}

	if maxExpiry := now.Add(maxSessionDuration); expiresAt.After(maxExpiry) {
		expiresAt = maxExpiry
	}

	session := sessionClaims{
		Subject:   idToken.Subject,
		Email:     claims.Email,
		Name:      name,
		ExpiresAt: expiresAt.Unix(),
	}

	value, err := a.encrypt(sessionPurpose, session)
	if err != nil {
		http.Error(w, "failed to establish session", http.StatusInternalServerError)

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})

	redirectPath := state.RedirectPath
	if redirectPath == "" {
		redirectPath = "/"
	}

	http.Redirect(w, r, redirectPath, http.StatusFound)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Authenticator) handleMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Never let a shared cache hand one operator's identity to another.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(identity); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// consumeStateCookie always clears the single-use state cookie so a replayed
// callback cannot reuse it.
func (a *Authenticator) consumeStateCookie(w http.ResponseWriter, r *http.Request) (stateClaims, bool) {
	clearCookie(w, r, stateCookieName)

	cookie, err := r.Cookie(stateCookieName)
	if err != nil {
		return stateClaims{}, false
	}

	var claims stateClaims

	if err := a.decrypt(statePurpose, cookie.Value, &claims); err != nil {
		return stateClaims{}, false
	}

	if time.Now().Unix() >= claims.ExpiresAt {
		return stateClaims{}, false
	}

	return claims, true
}

func (a *Authenticator) identityFromSessionCookie(r *http.Request) (Identity, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Identity{}, false
	}

	var claims sessionClaims

	if err := a.decrypt(sessionPurpose, cookie.Value, &claims); err != nil {
		return Identity{}, false
	}

	if time.Now().Unix() >= claims.ExpiresAt {
		return Identity{}, false
	}

	return Identity{Subject: claims.Subject, Email: claims.Email, Name: claims.Name}, true
}

// IdentityFromRequest returns the Identity in r's valid session cookie, for
// callers outside Wrap (such as the access log) that can't use
// IdentityFromContext.
func (a *Authenticator) IdentityFromRequest(r *http.Request) (Identity, bool) {
	return a.identityFromSessionCookie(r)
}

const loginPath = "/login"

// Error codes matching web/src/LoginPage.tsx's error states.
const (
	loginErrorDenied  = "denied"
	loginErrorExpired = "expired"
)

// rejectCallback records reason in the access log (the failure is otherwise
// an opaque 302) and redirects to the login page.
func rejectCallback(w http.ResponseWriter, r *http.Request, code, redirectPath, reason string, extra ...slog.Attr) {
	webserver.Annotate(r.Context(), append([]slog.Attr{slog.String("auth_error", reason)}, extra...)...)
	loginErrorRedirect(w, r, code, redirectPath, "")
}

// loginErrorRedirect sends the browser to the login page with code, the
// original destination, and the sanitized IdP detail. The SPA must still treat
// detail as untrusted: the /login URL can be crafted directly.
func loginErrorRedirect(w http.ResponseWriter, r *http.Request, code, redirectPath, detail string) {
	target := url.URL{Path: loginPath}

	query := url.Values{}
	query.Set("error", code)

	if redirectPath != "" && redirectPath != "/" {
		query.Set("redirect", redirectPath)
	}

	if detail != "" {
		query.Set("error_description", detail)
	}

	target.RawQuery = query.Encode()

	http.Redirect(w, r, target.String(), http.StatusFound)
}

// idpErrorDescriptionMaxLen bounds, in runes, the attacker-controllable
// error_description reflected onto the login page.
const idpErrorDescriptionMaxLen = 300

// sanitizeIdPErrorDescription reduces an untrusted error_description to one
// bounded line, dropping non-printable runes (including bidi controls).
func sanitizeIdPErrorDescription(description string) string {
	collapsed := strings.Join(strings.Fields(description), " ")

	var builder strings.Builder

	count := 0

	for _, r := range collapsed {
		if !unicode.IsPrint(r) {
			continue
		}

		builder.WriteRune(r)

		count++
		if count >= idpErrorDescriptionMaxLen {
			break
		}
	}

	return builder.String()
}

type buildInfoResponse struct {
	Version    string `json:"version"`
	FooterHost string `json:"footerHost,omitempty"`
}

func (a *Authenticator) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(buildInfoResponse{Version: a.appVersion, FooterHost: a.footerHost}); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// sanitizeRedirectPath restricts a post-login redirect to a same-origin path,
// else "/". It rejects "//host" and any backslash, which browsers normalize
// to "/", making "/\host" an equivalent open redirect. It also rejects ASCII
// control characters: browsers strip tab, CR and LF, so "/\t/host" becomes
// "//host" (issue #383).
func sanitizeRedirectPath(path string) string {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsRune(path, '\\') ||
		strings.ContainsFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "/"
	}

	// Redirecting to the login page would strand an authenticated user there
	// (mirrored in web/src/route.ts); the SPA treats "/login/" the same.
	pathname := path
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		pathname = path[:i]
	}

	if pathname == "/login" || pathname == "/login/" {
		return "/"
	}

	return path
}

// randomToken returns a crypto/rand URL-safe token for state and nonce.
func randomToken() string {
	return oauth2.GenerateVerifier()
}

// isTLS honours X-Forwarded-Proto: cmd/web sits behind a TLS-terminating
// ingress, so r.TLS alone would never set the Secure cookie attribute.
func isTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clearCookie must match the Secure attribute the cookie was set with, or
// browsers refuse to clear it ("Leave Secure Cookies Alone").
func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// writeJSONError mirrors pkg/runsapi's {"error": ...} envelope.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
