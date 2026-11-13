package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

const managementCookie = "gpuworks_session"

// sessionCookie is the management session cookie's name (SessionPolicy.Cookie).
func (a *apiHandlers) sessionCookie() string {
	if a.sessionPolicy.Cookie != "" {
		return a.sessionPolicy.Cookie
	}
	return managementCookie
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *apiHandlers) login(w http.ResponseWriter, r *http.Request) {
	if !a.managementOriginAllowed(r) {
		writeProductError(w, http.StatusForbidden, ErrorManagementOriginRejected, "cross-origin management request rejected", nil)
		return
	}
	if a.d.AuthSigner == nil || a.d.Store == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorAuthNotConfigured, "management auth is not configured", nil)
		return
	}
	var req loginRequest
	if !decodeJSONBody(w, r, &req) || !validateTextField(w, "username", req.Username, deployment.MaxNameRunes) {
		return
	}
	finish, wait := a.loginThrottle.begin(time.Now().UTC(), a.loginClientIP(r), req.Username)
	if wait > 0 {
		loginThrottled(w, wait)
		return
	}
	u, err := a.d.Store.AuthenticateUser(strings.TrimSpace(req.Username), req.Password)
	finish(err == nil, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		a.usageCounters().Inc(usagereport.KindLogin, "failed")
		writeProductError(w, http.StatusUnauthorized, ErrorInvalidCredentials, "invalid username or password", nil)
		return
	}
	if err != nil {
		a.writeManagementAuthError(w, err)
		return
	}
	policy := a.sessionPolicy.Normalized()
	credential, session, err := a.d.Store.CreateManagementSession(u, policy.IdleTimeout, policy.MaxLifetime)
	if err != nil {
		a.writeManagementAuthError(w, err)
		return
	}
	// Preserve the Bearer response for non-browser clients, bound to the same
	// revocable session. The browser receives only an opaque cookie.
	token, err := a.d.AuthSigner.Sign(auth.Claims{UserID: u.ID, Username: u.Username, Role: u.Role, SessionID: session.TokenHash})
	if err != nil {
		_ = a.d.Store.DeleteManagementSession(session.TokenHash)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.setManagementCookie(w, r, credential, session.AbsoluteExpiresAt)
	a.usageCounters().Inc(usagereport.KindLogin, "ok")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
	}})
}

func (a *apiHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if !a.managementOriginAllowed(r) {
		writeProductError(w, http.StatusForbidden, ErrorManagementOriginRejected, "cross-origin management request rejected", nil)
		return
	}
	if a.d.Store == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorAuthNotConfigured, "management auth is not configured", nil)
		return
	}
	// Revoke every presented credential, even if a stale Authorization header
	// would otherwise hide the browser cookie under normal Bearer precedence.
	hashes := []string{}
	if cookie, err := r.Cookie(a.sessionCookie()); err == nil && len(cookie.Value) == 64 {
		hashes = append(hashes, store.HashToken(cookie.Value))
	}
	if hash, err := a.managementSessionHash(r); err == nil && (len(hashes) == 0 || hashes[0] != hash) {
		hashes = append(hashes, hash)
	}
	for _, hash := range hashes {
		if a.d.Store == nil {
			break
		}
		if err := a.d.Store.DeleteManagementSession(hash); err != nil {
			writeErr(w, http.StatusServiceUnavailable, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: a.sessionCookie(), Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode, Secure: a.managementCookieSecure(r)})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (a *apiHandlers) me(w http.ResponseWriter, r *http.Request) {
	id, err := a.authenticateIdentity(r)
	if err != nil {
		a.writeManagementAuthError(w, err)
		return
	}
	claims := id.claims
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id": claims.UserID, "username": claims.Username, "role": claims.Role, "password_policy": passwordPolicy()})
}

// activity reports browser interaction. Passive GET polling and connection
// keepalives do not extend inactivity deadlines.
func (a *apiHandlers) activity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) managementSessionHash(r *http.Request) (string, error) {
	if bearer := auth.BearerToken(r.Header.Get("Authorization")); bearer != "" {
		if a.d.AuthSigner == nil {
			return "", auth.ErrInvalidToken
		}
		claims, err := a.d.AuthSigner.Verify(bearer)
		if err != nil {
			return "", err
		}
		if claims.SessionID == "" {
			return "", auth.ErrInvalidToken
		}
		return claims.SessionID, nil
	}
	cookie, err := r.Cookie(a.sessionCookie())
	if err != nil || cookie.Value == "" || len(cookie.Value) != 64 {
		return "", auth.ErrInvalidToken
	}
	return store.HashToken(cookie.Value), nil
}

func (a *apiHandlers) authenticateManagement(r *http.Request) (auth.Claims, *store.ManagementSession, error) {
	if a.d.Store == nil {
		return auth.Claims{}, nil, auth.ErrInvalidToken
	}
	hash, err := a.managementSessionHash(r)
	if err != nil {
		return auth.Claims{}, nil, err
	}
	u, session, err := a.d.Store.ManagementSessionByHash(hash)
	if err != nil {
		return auth.Claims{}, nil, err
	}
	if bearer := auth.BearerToken(r.Header.Get("Authorization")); bearer != "" {
		claims, err := a.d.AuthSigner.Verify(bearer)
		if err != nil {
			return auth.Claims{}, nil, err
		}
		if claims.UserID != u.ID {
			return auth.Claims{}, nil, auth.ErrInvalidToken
		}
	}
	return auth.Claims{UserID: u.ID, Username: u.Username, Role: u.Role, SessionID: hash}, session, nil
}

func (a *apiHandlers) claimsFromRequest(r *http.Request) (auth.Claims, bool) {
	if id, ok := r.Context().Value(identityKey{}).(identity); ok {
		return id.claims, true
	}
	id, err := a.authenticateIdentity(r)
	return id.claims, err == nil
}

func (a *apiHandlers) managementStreamAllowed(r *http.Request, admin bool) bool {
	id, err := a.authenticateIdentity(r)
	return err == nil && (!admin || id.claims.Role == "admin")
}

func (a *apiHandlers) writeManagementAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, auth.ErrInvalidToken) || errors.Is(err, auth.ErrExpiredToken) {
		writeProductError(w, http.StatusUnauthorized, ErrorAuthenticationRequired, "authentication required", nil)
		return
	}
	writeErr(w, http.StatusServiceUnavailable, err)
}

func (a *apiHandlers) managementAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.authenticateIdentity(r)
		if err != nil {
			a.writeManagementAuthError(w, err)
			return
		}
		if id.token == nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !a.managementOriginAllowed(r) {
				writeProductError(w, http.StatusForbidden, ErrorManagementOriginRejected, "cross-origin management request rejected", nil)
				return
			}
			session, err := a.d.Store.RenewManagementSession(id.claims.SessionID, a.sessionPolicy.Normalized().IdleTimeout)
			if err != nil {
				a.writeManagementAuthError(w, err)
				return
			}
			if auth.BearerToken(r.Header.Get("Authorization")) == "" {
				cookie, _ := r.Cookie(a.sessionCookie())
				a.setManagementCookie(w, r, cookie.Value, session.AbsoluteExpiresAt)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

func (a *apiHandlers) managementAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.requireManagementAdmin(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

func (a *apiHandlers) requireManagementAdmin(w http.ResponseWriter, r *http.Request) bool {
	id, err := a.authenticateIdentity(r)
	if err != nil {
		a.writeManagementAuthError(w, err)
		return false
	}
	if id.claims.Role != "admin" {
		writeProductError(w, http.StatusForbidden, ErrorAdministratorRequired, "administrator role required", nil)
		return false
	}
	return true
}

func (a *apiHandlers) managementOriginAllowed(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	} // Non-browser clients do not send Origin.
	u, err := url.Parse(origin)
	base, baseErr := url.Parse(a.managementRequestOrigin(r))
	return err == nil && baseErr == nil && u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func (a *apiHandlers) managementCookieSecure(r *http.Request) bool {
	base, err := url.Parse(a.managementRequestOrigin(r))
	return r.TLS != nil || (err == nil && base.Scheme == "https")
}

func (a *apiHandlers) setManagementCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: a.sessionCookie(), Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: a.managementCookieSecure(r), Expires: expires, MaxAge: maxAge})
}

// identity is who a management request acts as, resolved once by
// managementAuth: a login session, or a platform token and its user.
type identity struct {
	claims auth.Claims
	token  *store.PlatformToken
}

type identityKey struct{}

// requestToken is the platform token a request authenticated with, or nil
// for a login session.
func requestToken(r *http.Request) *store.PlatformToken {
	id, _ := r.Context().Value(identityKey{}).(identity)
	return id.token
}

// authenticateIdentity revalidates login sessions and uses the platform-token
// verifier, including on streams where a request's identity can be stale.
func (a *apiHandlers) authenticateIdentity(r *http.Request) (identity, error) {
	if bearer := auth.BearerToken(r.Header.Get("Authorization")); auth.IsPlatformToken(bearer) {
		if a.d.Store == nil {
			return identity{}, auth.ErrInvalidToken
		}
		token, user, ok := a.d.Store.VerifyPlatformToken(bearer)
		if !ok {
			return identity{}, auth.ErrInvalidToken
		}
		return identity{claims: auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role}, token: token}, nil
	}
	claims, _, err := a.authenticateManagement(r)
	return identity{claims: claims}, err
}

// sessionOnly refuses platform tokens: what it guards is managed from a
// login session.
func sessionOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := requestToken(r); t != nil {
			writeProductError(w, http.StatusForbidden, ErrorPlatformTokenNotAllowed, "platform tokens are managed from a login session", map[string]any{"scope": t.Scope})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readTokensRead lets a read token only read: GET, HEAD and OPTIONS.
func readTokensRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := requestToken(r); t != nil && t.Scope != store.PlatformTokenWrite {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				writeProductError(w, http.StatusForbidden, ErrorPlatformTokenReadOnly, "a read token can only read", map[string]any{"scope": t.Scope})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withClaims hands the signed-in user's claims to next through the request
// context, so the playground's request records name the user.
func (a *apiHandlers) withClaims(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claims, ok := a.claimsFromRequest(r); ok {
			r = r.WithContext(auth.WithClaims(r.Context(), claims))
		}
		next.ServeHTTP(w, r)
	})
}

// managementRequestOrigin trusts forwarded origin headers only from a
// trusted proxy peer, keeping cookie security and Origin checks aligned.
func (a *apiHandlers) managementRequestOrigin(r *http.Request) string {
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if a.sessionPolicy.Proxies.Peer(r) {
		if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
			host = forwarded
		}
		host = withForwardedPort(host, scheme, r.Header.Get("X-Forwarded-Port"))
	}
	return scheme + "://" + host
}

// withForwardedPort restores the port a proxy dropped from the host: nginx's
// $host carries no port, so behind "listen 8808" the browser's Origin has
// :8808 while the forwarded host does not. A host that names a port keeps it,
// and the scheme's default port is left implicit, as browsers write Origin.
func withForwardedPort(host, scheme, forwardedPort string) string {
	port, err := strconv.Atoi(strings.TrimSpace(forwardedPort))
	if err != nil || port < 1 || port > 65535 || host == "" {
		return host
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		return host
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), strconv.Itoa(port))
}
