package messages

// ErrorAgentDuplicateInstance: two agent processes present one node's
// identity and keep replacing each other's connection. The hub logs it
// beside the node and both agents' hosts and addresses.
const ErrorAgentDuplicateInstance = "AGENT_DUPLICATE_INSTANCE"
