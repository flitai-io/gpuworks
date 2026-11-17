package messages

// Node diagnosis codes (diagnostics.DoctorCheck.Code): what one stage of a
// node diagnosis found, or why it did not run. The findings an agent reports
// carry the proto Doctor* and accelerator health codes instead.
const (
	// Agent self-check stages.
	DoctorInformationCollected = "INFORMATION_COLLECTED"
	DoctorNotReported          = "NOT_REPORTED"
	DoctorRequiresAgent        = "REQUIRES_AGENT"

	// Access inspection over SSH.
	DoctorAdministratorRequired  = "ADMINISTRATOR_REQUIRED"
	DoctorCredentialsUnavailable = "CREDENTIALS_UNAVAILABLE"
	DoctorCredentialError        = "CREDENTIAL_ERROR"
	DoctorSSHUnavailable         = "SSH_UNAVAILABLE"
	DoctorSSHConnected           = "SSH_CONNECTED"
	DoctorInspectionUnavailable  = "INSPECTION_UNAVAILABLE"
	DoctorInstalled              = "INSTALLED"
	DoctorNotInstalled           = "NOT_INSTALLED"
	DoctorRunning                = "RUNNING"
	DoctorNotRunning             = "NOT_RUNNING"
	DoctorIdentityUnverified     = "IDENTITY_UNVERIFIED"

	// Connection establishment and authentication.
	DoctorInvalidAddress           = "INVALID_ADDRESS"
	DoctorTCPConnected             = "TCP_CONNECTED"
	DoctorHTTPConnected            = "HTTP_CONNECTED"
	DoctorHTTPError                = "HTTP_ERROR"
	DoctorDNS                      = "DNS"
	DoctorConnect                  = "CONNECT"
	DoctorRefused                  = "REFUSED"
	DoctorTLS                      = "TLS"
	DoctorTimeout                  = "TIMEOUT"
	DoctorCancelled                = "CANCELLED"
	DoctorTransport                = "TRANSPORT"
	DoctorToolMissing              = "TOOL_MISSING"
	DoctorAuthenticationNotChecked = "AUTHENTICATION_NOT_CHECKED"
)
