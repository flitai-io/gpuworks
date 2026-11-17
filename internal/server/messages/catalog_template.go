package messages

// Saving a deployment as an inference template
// (POST /api/v1/deployments/{id}/catalog-template, see
// docs/how-to/save-deployment-as-template.md).
const (
	// ErrorTemplateExportUnavailable reports a deployment that cannot be
	// saved as a template; params.reason says why.
	ErrorTemplateExportUnavailable = "TEMPLATE_EXPORT_UNAVAILABLE"
	// ErrorLaunchNotExportable reports a launch configuration whose command
	// line cannot be reproduced from catalog args ({engine}).
	ErrorLaunchNotExportable = "LAUNCH_NOT_EXPORTABLE"
)

// Operation checks of a template export (ADR 0067). Each names the operation
// ({operation}) and, when the request was answered, {http_status}; the
// gateway's own error text travels as the check's evidence.
const (
	// CheckTemplateOperationAnswered: the operation answered with a 2xx.
	CheckTemplateOperationAnswered = "TEMPLATE_OPERATION_ANSWERED"
	// CheckTemplateOperationHTTPError: the operation answered with a status
	// other than 2xx.
	CheckTemplateOperationHTTPError = "TEMPLATE_OPERATION_HTTP_ERROR"
	// CheckTemplateOperationNoReplica: no running replica took the request.
	CheckTemplateOperationNoReplica = "TEMPLATE_OPERATION_NO_REPLICA"
	// CheckTemplateOperationTimedOut: the request did not finish within the
	// export's bound (or the export was cancelled).
	CheckTemplateOperationTimedOut = "TEMPLATE_OPERATION_TIMED_OUT"
	// CheckTemplateOperationReplyIncomplete: the request failed, or a 2xx
	// reply broke off after its headers, so its status proves nothing.
	CheckTemplateOperationReplyIncomplete = "TEMPLATE_OPERATION_REPLY_INCOMPLETE"
	// CheckTemplateOperationNotProbed (skip): no test request is sent for this
	// operation, because one is slow or costly (video, image, speech).
	CheckTemplateOperationNotProbed = "TEMPLATE_OPERATION_NOT_PROBED"
	// CheckTemplateOperationNoEndpoint (skip): the operation declares no
	// endpoint a request could be sent to.
	CheckTemplateOperationNoEndpoint = "TEMPLATE_OPERATION_NO_ENDPOINT"
)
