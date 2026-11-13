package app

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// bootstrapAdminPassword resolves the password used when the admin account is
// created, and reports where it came from so startup can log the right thing.
//
// Precedence: Config.AdminPassword (in-process harnesses) >
// GPUWORKS_ADMIN_PASSWORD (a deployment-time choice, e.g. in a container) > the
// credentials file written by `gpuworks install` > "" (let the store generate
// one). Sources are "config", "env", "file" and "generated".
func bootstrapAdminPassword(cfg Config, policies ...securefile.Policy) (string, string) {
	if cfg.AdminPassword != "" {
		return cfg.AdminPassword, "config"
	}
	if pw := strings.TrimSpace(os.Getenv(adminPasswordEnvVar)); pw != "" {
		return pw, "env"
	}
	if creds, err := installcred.ReadInitialCredentials(cfg.DataDir, policies...); err == nil && creds.Password != "" {
		return creds.Password, "file"
	}
	return "", "generated"
}

// adminPasswordEnvVar names the initial admin password on first start. It is
// read only when the admin account is created, never afterwards.
const adminPasswordEnvVar = "GPUWORKS_ADMIN_PASSWORD"

// bootstrapAdminAccount creates the initial admin account and a default API
// key when the store has neither yet, so /v1 is usable immediately after
// first start. The password comes from the configuration,
// GPUWORKS_ADMIN_PASSWORD, the credentials file written by `gpuworks install`, or the store's generator;
// either way it is recorded in the credentials file, which is the durable
// place to read it. Passwords are never copied into the server log.
func bootstrapAdminAccount(cfg Config, st *store.Store, log *slog.Logger, policies ...securefile.Policy) error {
	bootstrapPassword, passwordSource := bootstrapAdminPassword(cfg, policies...)
	username, initialPassword, created, err := st.EnsureAdmin(cfg.AdminUser, bootstrapPassword)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	if created {
		credsPath := installcred.CredentialsPath(cfg.DataDir)
		if err := installcred.WriteInitialCredentials(cfg.DataDir, username, initialPassword, policies...); err != nil {
			return fmt.Errorf("record initial admin credentials in %s (use gpuworks reset-password to recover the account): %w", credsPath, err)
		}
		switch passwordSource {
		case "config":
			log.Info("created admin user with the configured password", "user", username)
		case "env":
			log.Info("created admin user with the password from "+adminPasswordEnvVar, "user", username)
		case "file":
			log.Info("created admin user with the password from the install credentials file", "user", username, "path", credsPath)
		default:
			log.Info("created admin user with a generated password; read it from the private credentials file", "user", username, "path", credsPath)
			log.Info("to choose the initial admin password, set "+adminPasswordEnvVar+" or pass --password to gpuworks install before first start", "path", installcred.CredentialsPath(cfg.DataDir))
		}
		log.Info("initial admin credentials are recorded in this file; change the password and delete it after first login",
			"path", credsPath)
	}
	// The gateway requires an API key unless an administrator opened it. Tell
	// an operator who has none, because /v1 will refuse every request until
	// they create one; do not mint a key here, since its secret would be
	// discarded and nothing could authenticate with it.
	if open, err := st.InferenceOpenAccess(); err == nil && !open && !hasLiveAPIKey(st) {
		log.Info("the gateway requires an API key and no API token exists yet; create one under API tokens, or turn on open access under Settings > Security")
	}
	return nil
}

// hasLiveAPIKey reports whether an unrevoked API key exists: a best-effort read
// for the startup hint, not a security decision.
func hasLiveAPIKey(st *store.Store) bool {
	keys, err := st.ListAPIKeys()
	if err != nil {
		return true // Unknown: stay quiet rather than hint wrongly.
	}
	for i := range keys {
		if keys[i].RevokedAt == nil {
			return true
		}
	}
	return false
}
