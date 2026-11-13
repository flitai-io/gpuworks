package api

import (
	"errors"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword replaces the signed-in user's own password. The current
// password is checked under the login throttle, so a borrowed session cannot
// be used to guess it faster than the sign-in page allows. Every session of
// the user ends, this one included; API keys and platform tokens stay.
func (a *apiHandlers) changePassword(w http.ResponseWriter, r *http.Request) {
	if !a.managementOriginAllowed(r) {
		writeProductError(w, http.StatusForbidden, ErrorManagementOriginRejected, "cross-origin management request rejected", nil)
		return
	}
	claims, ok := a.claimsFromRequest(r)
	if !ok {
		writeProductError(w, http.StatusUnauthorized, ErrorAuthenticationRequired, "authentication required", nil)
		return
	}
	var req changePasswordRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// Both passwords are judged on every submission, so the form can mark
	// both fields at once instead of revealing one problem per attempt.
	policyErr := auth.ValidateAccountPassword(claims.Username, req.NewPassword)
	finish, wait := a.loginThrottle.begin(time.Now().UTC(), a.loginClientIP(r), claims.Username)
	if wait > 0 {
		loginThrottled(w, wait)
		return
	}
	_, err := a.d.Store.AuthenticateUser(claims.Username, req.CurrentPassword)
	finish(err == nil, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		var params map[string]any
		if policyErr != nil {
			params = passwordPolicyParams()
			params["new_password"] = passwordPolicyCode(policyErr)
		}
		writeProductError(w, http.StatusBadRequest, ErrorCurrentPasswordInvalid, "the current password is wrong", params)
		return
	}
	if err != nil {
		a.writeManagementAuthError(w, err)
		return
	}
	if policyErr != nil {
		writePasswordPolicyError(w, policyErr)
		return
	}
	// Judged after the current password is known to be right, so a typo there
	// is reported as such rather than as an unchanged password.
	if req.NewPassword == req.CurrentPassword {
		writeProductError(w, http.StatusBadRequest, ErrorPasswordUnchanged, "the new password is the current password", nil)
		return
	}
	if err := a.d.Store.SetUserPassword(claims.Username, req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrPasswordPolicy) {
			writePasswordPolicyError(w, err)
			return
		}
		a.writeManagementAuthError(w, err)
		return
	}
	// The install-time record now holds a password that no longer works.
	if a.d.ForgetInitialCredentials != nil {
		if err := a.d.ForgetInitialCredentials(claims.Username); err != nil && a.d.Log != nil {
			a.d.Log.Warn("initial admin credentials file not removed after a password change; delete it by hand", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: a.sessionCookie(), Value: "", Path: "/", HttpOnly: true, MaxAge: -1, SameSite: http.SameSiteLaxMode, Secure: a.managementCookieSecure(r)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "password_changed"})
}

// passwordPolicy is the rule a new password must meet, for the change form
// to state before the server is asked.
func passwordPolicy() map[string]int {
	return map[string]int{"min_length": auth.MinPasswordRunes, "max_length": auth.MaxPasswordRunes}
}

// passwordPolicyErrors names each policy reason's product error.
var passwordPolicyErrors = map[string]string{
	auth.PolicyLength:     ErrorPasswordLength,
	auth.PolicyCharacters: ErrorPasswordCharacters,
	auth.PolicyGuessable:  ErrorPasswordGuessable,
	auth.PolicyUsername:   ErrorPasswordUsername,
}

// passwordPolicyCode is the product error naming why err rejected a password.
func passwordPolicyCode(err error) string {
	var policy *auth.PolicyError
	if errors.As(err, &policy) && passwordPolicyErrors[policy.Reason] != "" {
		return passwordPolicyErrors[policy.Reason]
	}
	return ErrorPasswordGuessable
}

func passwordPolicyParams() map[string]any {
	return map[string]any{"min": auth.MinPasswordRunes, "max": auth.MaxPasswordRunes}
}

func writePasswordPolicyError(w http.ResponseWriter, err error) {
	writeProductError(w, http.StatusBadRequest, passwordPolicyCode(err), err.Error(), passwordPolicyParams())
}
