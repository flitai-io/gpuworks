package messages

const (
	// ZStack provider validation errors; classification belongs to the Cloud driver.
	ErrorZStackDNSFailed                    = "ZSTACK_DNS_FAILED"
	ErrorZStackIPUnreachable                = "ZSTACK_IP_UNREACHABLE"
	ErrorZStackPortRefused                  = "ZSTACK_PORT_REFUSED"
	ErrorZStackRequestFailed                = "ZSTACK_REQUEST_FAILED"
	ErrorZStackRequestTimeout               = "ZSTACK_REQUEST_TIMEOUT"
	ErrorZStackAuthRejected                 = "ZSTACK_AUTH_REJECTED"
	ErrorZStackAccessKeyInvalid             = "ZSTACK_ACCESS_KEY_INVALID"
	ErrorZStackSignatureInvalid             = "ZSTACK_SIGNATURE_INVALID"
	ErrorZStackLicenseRequired              = "ZSTACK_LICENSE_REQUIRED"
	ErrorZStackClockSkew                    = "ZSTACK_CLOCK_SKEW"
	ErrorZStackProxyConnectionFailed        = "ZSTACK_PROXY_CONNECTION_FAILED"
	ErrorZStackTLSFailed                    = "ZSTACK_TLS_FAILED"
	ErrorZStackResponseTimeout              = "ZSTACK_RESPONSE_TIMEOUT"
	ErrorZStackConnectionRecovered          = "ZSTACK_CONNECTION_RECOVERED"
	ErrorZStackPortUnreachable              = "ZSTACK_PORT_UNREACHABLE"
	ErrorZStackNetworkUnconfirmed           = "ZSTACK_NETWORK_UNCONFIRMED"
	ErrorZStackNetworkDiagnosticUnavailable = "ZSTACK_NETWORK_DIAGNOSTIC_UNAVAILABLE"
)

// Refusals from Cloud host inventory checks before agent installation.
const (
	ErrorZStackAgentHostType           = "ZSTACK_AGENT_HOST_TYPE"
	ErrorZStackAgentHostHypervisor     = "ZSTACK_AGENT_HOST_HYPERVISOR"
	ErrorZStackAgentHostAddressMissing = "ZSTACK_AGENT_HOST_ADDRESS_MISSING"
	ErrorZStackAgentHostNotConnected   = "ZSTACK_AGENT_HOST_NOT_CONNECTED"
)
