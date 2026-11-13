package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// AuthorizePublicKey uses Cloud's existing host SSH access. KvmRunShell runs
// as the host inventory's SSH user, also used by cloudHostInstance.
func (d *Driver) AuthorizePublicKey(ctx context.Context, creds provider.Credentials, externalID, publicKey string) (string, error) {
	if strings.TrimSpace(externalID) == "" {
		return "", fmt.Errorf("zstack: a target host is required for SSH key authorization")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil || len(options) != 0 || len(strings.TrimSpace(string(rest))) != 0 {
		return "", fmt.Errorf("zstack: invalid SSH public key")
	}
	c, err := d.newClient(creds)
	if err != nil {
		return "", err
	}
	var request struct {
		RunShell struct {
			HostUUIDs []string `json:"hostUuids"`
			Script    string   `json:"script"`
		} `json:"kvmRunShell"`
	}
	request.RunShell.HostUUIDs = []string{externalID}
	script := hostPublicKeyScript(key)
	// Cloud's unquoted heredoc expands shell variables before the script runs.
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	request.RunShell.Script = "set -o pipefail\nprintf '%s' '" + encoded + "' | base64 -d | bash"
	var reply struct {
		Inventory map[string]*struct {
			ReturnCode *int            `json:"returnCode"`
			ErrorCode  json.RawMessage `json:"errorCode"`
		} `json:"inventory"`
	}
	if err := c.PutActionResult(ctx, "/hosts/kvm/actions", request, &reply); err != nil {
		return "", fmt.Errorf("zstack: SSH key authorization failed: %w", err)
	}
	result := reply.Inventory[externalID]
	if result == nil || result.ReturnCode == nil {
		return "", fmt.Errorf("zstack: missing host SSH key authorization result")
	}
	if len(result.ErrorCode) != 0 && string(result.ErrorCode) != "null" {
		return "", fmt.Errorf("zstack: Cloud could not execute SSH key authorization on the host")
	}
	if *result.ReturnCode != 0 {
		return "", fmt.Errorf("zstack: host SSH key authorization exited with code %d", *result.ReturnCode)
	}
	return "authorized the platform key on the selected Cloud host", nil
}

var _ provider.PublicKeyAuthorizer = (*Driver)(nil)

func hostPublicKeyScript(key ssh.PublicKey) string {
	publicKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	return fmt.Sprintf(`set -e
umask 077
dir="$HOME/.ssh"
file="$dir/authorized_keys"
mkdir -p "$dir"
touch "$file"
if ! grep -qF -- '%s' "$file"; then
  if [ -s "$file" ] && [ -n "$(tail -c 1 "$file")" ]; then printf '\n' >> "$file"; fi
  %s
fi
chmod 700 "$dir"
chmod 600 "$file"`, publicKey, provision.AuthorizeKeyCommand(publicKey))
}
