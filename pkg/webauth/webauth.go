// Package webauth adds OIDC authorization-code-flow authentication to
// cmd/web: a login route that redirects to the configured identity
// provider, a callback route that exchanges the returned code, verifies the
// ID token, and establishes a session, a logout route that clears it, and
// session middleware gating every other route.
//
// The provider is discovered purely from its issuer URL (OIDC discovery,
// coreos/go-oidc/v3) plus a client ID/secret/redirect URL — nothing here is
// specific to any one identity provider (docs/web-ui-design.md §4, §6).
// Authentication only: any authenticated user is authorized, matching the
// design doc's scope — role/permission-based authorization is not
// implemented.
//
// Sessions are a stateless, encrypted, tamper-evident cookie (AES-256-GCM),
// not a server-side store — cmd/web stays stateless end to end
// (docs/web-ui-design.md §3; SPEC §4.2 forbids a UI-owned catalog/store). A
// second, short-lived cookie carries the CSRF state/nonce/PKCE verifier
// between the login redirect and the callback, since there is nowhere
// server-side to stash it between the two requests. Both cookies are
// encrypted with the same key but under different AAD "purposes", so one
// can never be replayed as the other.
//
// Unauthenticated page requests (everything except "/api/*") are served the
// SPA unconditionally rather than redirected — the SPA itself renders a
// styled login page (web/src/LoginPage.tsx) and calls GET /auth/login when
// the operator activates it, once it learns (from GET /api/me, still
// 401-gated) that it has no session. A failed OIDC callback (IdP denial,
// expired/invalid login state, a rejected ID token, ...) redirects back to
// that same login page with an "error" query parameter (loginErrorRedirect)
// the SPA renders as an error-denied/error-expired state, instead of the
// callback failing with a raw HTTP error page. "/api/*" requests are
// unaffected either way: a missing/invalid session there always gets a 401
// JSON body, never a redirect.
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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// sessionKeyLen is the required length, in bytes, of Config.SessionKey — 32
// bytes for AES-256.
const sessionKeyLen = 32

// stateCookieTTL bounds how long a login attempt (the redirect to the IdP
// and back) has to complete. It is deliberately short: the state cookie only
// needs to survive one round trip through the IdP's own login UI.
const stateCookieTTL = 10 * time.Minute

// maxSessionDuration caps how long an established session cookie stays
// valid, even if the IdP issued an ID token with a longer expiry — an
// operator UI session should not silently outlive a reasonable working
// session just because a provider is configured with a very long token
// lifetime.
const maxSessionDuration = 24 * time.Hour

// minSessionDuration floors the session lifetime, so a provider configured
// with short-lived ID tokens (5-60 min is common — an ID token is meant to
// bound the token, not a UI session) does not bounce an operator back to
// login part-way through a task. Once the encrypted session cookie is minted
// the ID token's own expiry is no longer consulted — there is no refresh flow
// and every request is authorized from the self-contained cookie — so the
// session lifetime is this service's own choice, clamped to
// [minSessionDuration, maxSessionDuration] regardless of the token's expiry.
// A working day keeps an operator signed in across a long-running backup's
// staging + eject-pause wait without re-auth, while staying well bounded.
const minSessionDuration = 8 * time.Hour

// discoveryTimeout bounds the OIDC discovery call New makes at startup — the
// same defensive pattern pkg/temporalclient.New uses for its own startup
// health check. Without it, a misconfigured or unreachable issuer would hang
// cmd/web's startup indefinitely: the main listener would never open, yet
// the health server (already serving /readyz by that point) has no way to
// know, so Kubernetes could keep routing traffic at a pod whose main port
// will never accept a connection.
const discoveryTimeout = 10 * time.Second

// Cookie names. Both are HttpOnly and scoped to the whole app (Path "/") so
// they are sent on every request, including the SPA's asset requests.
const (
	sessionCookieName = "ta_session"
	stateCookieName   = "ta_oidc_state"
)

// AAD "purposes" distinguishing the two cookie kinds under the same
// encryption key — see the package doc comment.
const (
	sessionPurpose = "session"
	statePurpose   = "state"
)

// Config configures an Authenticator. All fields are required; New returns
// an error if any are missing or malformed. cmd/web builds this from
// OIDC_ISSUER_URL / OIDC_CLIENT_ID / OIDC_CLIENT_SECRET / OIDC_REDIRECT_URL /
// WEB_SESSION_KEY (docs/configuration.md).
type Config struct {
	// IssuerURL is the OIDC provider's issuer URL, used for discovery
	// (GET {IssuerURL}/.well-known/openid-configuration).
	IssuerURL string
	// ClientID and ClientSecret identify this app to the provider as a
	// confidential client.
	ClientID     string
	ClientSecret string
	// RedirectURL is this app's callback URL as registered with the
	// provider, e.g. "https://tape-archiver.example.com/auth/callback".
	RedirectURL string
	// SessionKey is 32 bytes of key material (AES-256-GCM) used to encrypt
	// session and login-state cookies. See ParseSessionKey for turning the
	// WEB_SESSION_KEY environment variable into this. Losing/rotating this
	// key invalidates every outstanding session and in-flight login
	// attempt — acceptable, since the service is otherwise stateless and a
	// dropped session just means logging in again.
	SessionKey []byte
	// AppVersion is the tape-archiver build version (cmd/web passes
	// internal/buildinfo.ToolVersion()), served ungated at GET
	// /api/build-info for the login page's and sidebar's footer
	// (web/src/Footer.tsx). Never a hardcoded literal like the design's
	// sample "v0.4.1" — see docs/configuration.md.
	AppVersion string
	// FooterHost is an optional deploy-time label (e.g. a host or
	// deployment name — WEB_SESSION_KEY's sibling env var
	// WEB_FOOTER_HOST) appended to the footer version line. Empty by
	// default, in which case the footer omits that segment entirely rather
	// than showing a blank placeholder — see docs/configuration.md.
	FooterHost string
}

// Identity is the authenticated user, as returned by GET /api/me and
// attached to each gated request's context.
type Identity struct {
	// Subject is the OIDC "sub" claim: a stable, provider-scoped user
	// identifier. Always present.
	Subject string `json:"subject"`
	// Email and Name come from the ID token's "email"/"name" claims
	// (falling back to "preferred_username" for Name) when the provider
	// includes them; both are omitted from the JSON body when empty rather
	// than guessed at.
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Authenticator holds a configured OIDC client and the session-cookie
// cipher. Build one with New; mount it over the rest of the app with Wrap.
type Authenticator struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	gcm          cipher.AEAD
	appVersion   string
	footerHost   string
}

// ParseSessionKey decodes a base64-encoded 32-byte AES-256 key (e.g. the
// output of `openssl rand -base64 32`) for Config.SessionKey. cmd/web calls
// this on the WEB_SESSION_KEY environment variable.
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
// Authenticator. Discovery is a network call, so a shutdown signal
// cancelling ctx during it surfaces as ctx.Err(), same convention as
// pkg/temporalclient.New — callers should treat that as an orderly stop, not
// a startup failure.
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

// validateConfig checks that every Config field is present, so a
// misconfigured deployment fails at startup (New) rather than on first
// request.
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

// Wrap returns the handler cmd/web serves: the auth routes below, plus next
// (the SPA + /api/* mux built by pkg/webserver) gated behind a valid
// session. Auth routes are registered on a more specific pattern than "/",
// so http.ServeMux matches them ahead of the catch-all regardless of
// registration order.
//
//   - GET /auth/login — starts the OIDC flow: redirects to the provider's
//     authorization endpoint. Not gated (that would be circular).
//   - GET /auth/callback — the provider's redirect target: exchanges the
//     code, verifies the ID token, and sets the session cookie on success;
//     on failure, redirects to the SPA's login route with an "error" query
//     parameter (see loginErrorRedirect) instead of failing the request.
//     Not gated, for the same reason as /auth/login.
//   - GET /auth/logout — clears the session cookie and redirects to "/".
//     Not gated: logging out an already-logged-out session is a no-op, not
//     an error.
//   - GET /api/build-info — the build version and (if configured) footer
//     host label (buildInfoResponse, as JSON), for the login page's and
//     sidebar's footer (web/src/Footer.tsx). Not gated: it renders on the
//     unauthenticated login page, and a build version/deploy label is not
//     sensitive.
//   - GET /api/me — the authenticated identity (Identity, as JSON). Gated.
//   - everything else (next) — /api/* is gated: an unauthenticated request
//     there gets 401 with a JSON body. Every other path is served
//     unconditionally, authenticated or not (see requireSession) — the SPA
//     itself (web/src/App.tsx's AuthGate) decides whether to render the
//     styled login page or the real app shell, based on whether GET
//     /api/me reports a session.
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

// requireSession attaches the authenticated Identity to the request context
// (see IdentityFromContext) when a valid session cookie is present, then
// always forwards to next — except an unauthenticated request under
// "/api/", which gets a 401 JSON body instead: a fetch()/XHR caller cannot
// usefully follow a redirect into an HTML page, and next (the SPA) has
// nothing to serve an API caller anyway. Every other unauthenticated
// request (a page, or one of the SPA's own static assets) is served next
// exactly as an authenticated one would be — the SPA bundle itself carries
// no secrets, and it is what renders the styled login page; see the package
// doc comment.
func (a *Authenticator) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := a.identityFromSessionCookie(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")

				return
			}

			next.ServeHTTP(w, r)

			return
		}

		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
	})
}

// handleLogin starts the OIDC authorization-code flow: it mints CSRF
// state, an OIDC nonce, and a PKCE verifier, stashes all three (plus where
// to send the browser back to) in the short-lived, encrypted state cookie,
// then redirects to the provider's authorization endpoint.
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

// handleCallback is the provider's redirect target: it validates the state
// cookie and CSRF state, exchanges the authorization code (with the PKCE
// verifier from the state cookie), verifies the returned ID token's
// signature/issuer/audience/expiry (coreos/go-oidc) and nonce, and, on
// success, sets the session cookie and redirects to wherever the login
// attempt started from. On any failure it redirects to the SPA's login page
// with an "error" query parameter (loginErrorRedirect) instead of failing
// the request outright, so the operator always lands back on a page that
// explains what happened and offers to retry, never a bare HTTP error page
// (see the package doc comment).
//
// Two error codes are distinguished, matching web/src/LoginPage.tsx's
// error-denied/error-expired states: "denied" is only for an explicit
// denial reported by the IdP itself (the "error" query parameter);
// everything else this handler can reject — a missing/expired/tampered
// state cookie, a CSRF state mismatch, a missing code, a failed code
// exchange, a missing/invalid/expired ID token, a nonce mismatch — is
// reported as "expired", since from the operator's perspective all of these
// mean the same thing: the login attempt did not complete and they should
// just try again.
func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	state, ok := a.consumeStateCookie(w, r)
	if !ok {
		loginErrorRedirect(w, r, loginErrorExpired, "")

		return
	}

	query := r.URL.Query()

	if errParam := query.Get("error"); errParam != "" {
		loginErrorRedirect(w, r, loginErrorDenied, state.RedirectPath)

		return
	}

	returnedState := query.Get("state")
	if returnedState == "" || subtle.ConstantTimeCompare([]byte(returnedState), []byte(state.State)) != 1 {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	code := query.Get("code")
	if code == "" {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	token, err := a.oauth2Config.Exchange(r.Context(), code, oauth2.VerifierOption(state.PKCEVerifier))
	if err != nil {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(state.Nonce)) != 1 {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}

	if err := idToken.Claims(&claims); err != nil {
		loginErrorRedirect(w, r, loginErrorExpired, state.RedirectPath)

		return
	}

	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}

	// Clamp the session lifetime to [minSessionDuration, maxSessionDuration]:
	// cap an over-long token so a session cannot outlive a reasonable working
	// session, and floor a short-lived one so an IdP issuing 5-minute ID
	// tokens does not bounce the operator to login mid-task (see the duration
	// consts). The token's own expiry is only a starting hint here.
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

// handleLogout clears the session cookie and sends the browser back to "/",
// which (with no session left) immediately redirects to /auth/login again —
// so a caller does not need to special-case the post-logout landing page.
func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, sessionCookieName)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleMe implements GET /api/me: the caller's Identity, as attached to
// the request context by requireSession. It is only ever reached once
// requireSession has already confirmed a valid session (Wrap gates it), so
// the "missing identity" branch below is a defensive fallback, not a
// reachable API contract.
func (a *Authenticator) handleMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := IdentityFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(identity); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// consumeStateCookie reads and decrypts the login-state cookie set by
// handleLogin, clearing it either way (it is single-use: valid for exactly
// one callback, successful or not) so a replayed callback request can never
// reuse it.
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

// identityFromSessionCookie reads and decrypts the session cookie, if any.
// Any failure — no cookie, tampered/corrupt value, expired session — is
// reported the same way (ok == false): the caller cannot and should not
// distinguish "no session" from "invalid session", both just mean
// "not authenticated".
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

// loginPath is the SPA route the styled login page (web/src/LoginPage.tsx)
// is served at.
const loginPath = "/login"

// Error codes handleCallback passes to loginErrorRedirect, matching
// web/src/LoginPage.tsx's error-denied/error-expired states.
const (
	loginErrorDenied  = "denied"
	loginErrorExpired = "expired"
)

// loginErrorRedirect sends the browser to the SPA's login page with an
// "error" query parameter (loginErrorDenied/loginErrorExpired) the SPA
// renders as an error state, and — when known — a "redirect" parameter
// carrying the original login attempt's destination (already sanitized by
// sanitizeRedirectPath when handleLogin first set it in the state cookie),
// so a successful retry still lands wherever the operator originally meant
// to go. Called only by handleCallback; see its doc comment for which
// failures map to which code.
func loginErrorRedirect(w http.ResponseWriter, r *http.Request, code string, redirectPath string) {
	target := url.URL{Path: loginPath}

	query := url.Values{}
	query.Set("error", code)

	if redirectPath != "" && redirectPath != "/" {
		query.Set("redirect", redirectPath)
	}

	target.RawQuery = query.Encode()

	http.Redirect(w, r, target.String(), http.StatusFound)
}

// buildInfoResponse is GET /api/build-info's JSON body.
type buildInfoResponse struct {
	Version    string `json:"version"`
	FooterHost string `json:"footerHost,omitempty"`
}

// handleBuildInfo implements the ungated GET /api/build-info — see Wrap's
// doc comment.
func (a *Authenticator) handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(buildInfoResponse{Version: a.appVersion, FooterHost: a.footerHost}); err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
	}
}

// sanitizeRedirectPath restricts a caller-supplied post-login redirect to a
// same-origin absolute path, defaulting to "/" for anything else — in
// particular rejecting a protocol-relative "//attacker.example" value, which
// browsers treat as an absolute URL despite the leading "/", and any
// backslash, which browsers normalize to a forward slash for http(s) URLs
// (WHATWG URL spec) — so "/\attacker.example" resolves identically to
// "//attacker.example" and would otherwise be an equivalent open redirect
// that this function's "//" check alone does not catch.
func sanitizeRedirectPath(path string) string {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsRune(path, '\\') {
		return "/"
	}

	// Never redirect back to the login page itself: an authenticated user sent
	// to /login after a successful callback would be stranded there (the SPA
	// renders its login view for that route regardless of session, and its
	// AuthGate redirect does not re-fire) — see web/src/route.ts's mirror.
	pathname := path
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		pathname = path[:i]
	}

	// Trim a single trailing slash before comparing: the SPA's parseRoute
	// resolves both "/login" and "/login/" to the login route, so "/login/"
	// must be rejected here too or it slips through to strand the user.
	if pathname == "/login" || pathname == "/login/" {
		return "/"
	}

	return path
}

// randomToken returns a URL-safe random string suitable for CSRF state and
// OIDC nonce values — the same generator oauth2.GenerateVerifier() uses for
// PKCE verifiers (crypto/rand-backed, never fails), reused here rather than
// a second hand-rolled copy of the same "N random bytes, base64" primitive.
func randomToken() string {
	return oauth2.GenerateVerifier()
}

// isTLS reports whether r arrived over TLS, directly or via a reverse proxy.
// cmd/web never terminates TLS itself (docs/web-ui-design.md §5's
// TLS-terminating Ingress deployment model), so r.TLS is nil on every
// request the process ever sees in that topology; relying on it alone would
// mean the Secure cookie attribute below is never actually set in
// production. X-Forwarded-Proto is the standard signal a TLS-terminating
// proxy sets for exactly this — trusted here the same way ingress
// controllers/load balancers are already trusted for every other aspect of
// the connection (the pod is not expected to be reachable except through
// it).
func isTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clearCookie expires a cookie immediately, matching the same Path and
// Secure attributes it was set with (see isTLS) — a modern browser on a
// non-secure connection silently refuses to modify a cookie that has Secure
// set ("Leave Secure Cookies Alone"), so this must compute Secure the same
// way handleLogin/handleCallback did or a clear issued over plain HTTP could
// fail to remove a cookie that was set as Secure.
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

// writeJSONError writes a {"error": message} JSON body with the given
// status code, mirroring pkg/runsapi's error envelope so API responses look
// consistent regardless of which package under /api/ produced them.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
