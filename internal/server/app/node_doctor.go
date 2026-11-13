package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func newNodeDoctor(st *store.Store, h *hub.Hub, installer *provision.SSHInstaller) *diagnostics.DoctorService {
	return &diagnostics.DoctorService{
		Nodes: h,
		Settings: func(uuid string) (diagnostics.ConnectionNode, error) {
			node, err := st.NodeByUUID(uuid)
			if err != nil {
				return diagnostics.ConnectionNode{}, err
			}
			mode := node.ConnectMode
			if mode == "" {
				mode = agentconf.AgentDial
			}
			return diagnostics.ConnectionNode{UUID: uuid, Mode: mode, ServerURL: node.ServerURL, Address: node.ServerDialAddress, Token: node.Token}, nil
		},
		Connection: diagnostics.ConnectionDiagnoser{
			Inspect: func(ctx context.Context, uuid string) provision.AccessInspection {
				out := provision.AccessInspection{SSH: "unavailable", Installation: "unavailable", Process: "unavailable", Reason: "credentials_unavailable"}
				node, err := st.NodeByUUID(uuid)
				if err != nil || !node.HasSSHCredential {
					return out
				}
				row, secrets, err := st.GetNodeSSHCredential(uuid)
				if err != nil {
					out.Reason = "credential_error"
					return out
				}
				var platformKey *provision.PlatformKey
				if key, pem, keyErr := st.PlatformSSHKey(); keyErr == nil {
					platformKey = &provision.PlatformKey{PrivatePEM: pem, PublicKey: key.PublicKey, Fingerprint: key.Fingerprint}
				}
				return installer.InspectAccess(ctx, provision.SSHProbeRequest{Host: row.Host, Port: row.Port, User: row.User, Token: node.Token, InstallDir: node.InstallDir, ServerURL: node.ServerURL, ConnectMode: node.ConnectMode, SSHAuth: provision.SSHAuth{Password: secrets.Password, PrivateKeyPEM: secrets.PrivateKeyPEM, PlatformKey: platformKey}})
			},
		},
	}
}
