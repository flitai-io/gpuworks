package selfinstall

import (
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// Where a bootstrap admin password came from, reported to the operator so they
// know whether the server will accept the value shown.
const (
	PasswordFromFlag    = "flag"
	PasswordFromEnv     = "env"
	PasswordFromPrompt  = "prompt"
	PasswordGenerated   = "generated"
	PasswordUnchanged   = "unchanged"
	AdminPasswordEnvVar = "GPUWORKS_ADMIN_PASSWORD"
)

// resolvePassword decides the bootstrap admin password for a fresh install.
//
// Precedence: --password, then GPUWORKS_ADMIN_PASSWORD, then an interactive
// prompt, then a generated one. A quiet or non-interactive run never prompts,
// so an empty password there means "generate" rather than "ask".
//
// The password is returned to be written into <data>/initial-credentials; it is
// never written into the unit file, which is world-readable.
func (i *Installer) resolvePassword(req InstallRequest) (string, string, error) {
	if pw := strings.TrimSpace(req.Password); pw != "" {
		return checkedPassword(pw, PasswordFromFlag)
	}
	interactive := !req.Quiet && i.io.Terminal()
	if pw := strings.TrimSpace(os.Getenv(AdminPasswordEnvVar)); pw != "" {
		if !interactive {
			return checkedPassword(pw, PasswordFromEnv)
		}
		// The operator exported a password on purpose; show it back so a typo
		// in the environment does not become the password of a live server.
		i.io.Say("%s is set to: %s", AdminPasswordEnvVar, pw)
		use, err := i.io.Confirm("Use this as the admin password?", true)
		if err != nil {
			return "", "", err
		}
		if use {
			return checkedPassword(pw, PasswordFromEnv)
		}
	}
	if !interactive {
		return generatedPassword()
	}
	pw, err := i.io.PromptPassword("Admin password (default: random): ")
	if err != nil {
		return "", "", err
	}
	if pw == "" {
		return generatedPassword()
	}
	return checkedPassword(pw, PasswordFromPrompt)
}

func checkedPassword(password, source string) (string, string, error) {
	if err := auth.ValidateAccountPassword(DefaultAdminUser, password); err != nil {
		return "", "", err
	}
	return password, source, nil
}

// generatedPassword returns a random bootstrap password. It uses the same
// generator the store falls back to, so a password minted here is
// indistinguishable from one the server would have created itself.
func generatedPassword() (string, string, error) {
	password, err := auth.NewPassword()
	if err != nil {
		return "", "", err
	}
	return password, PasswordGenerated, nil
}
