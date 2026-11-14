// Package deployment holds the deployment use cases: a request comes in,
// and a plan, a deployment row or a ProductError comes out. Nothing here knows
// about net/http. The HTTP handlers in internal/server/api decode a request,
// call a use case and map its answer onto a status and a DTO; the automation,
// batches and the MCP tools call the same use cases instead of going through
// HTTP or keeping a copy of the logic.
//
// Today it holds creating and evaluating a deployment, with every step they
// run - resolving the template and its weights, admitting the model, planning
// placement, preparing the image and the shared download - and the
// deployment read model the list, the monitor and the batches share
// (StoreRows, ListRows, and EffectiveState, the one state every page shows
// for a deployment). The requests and previews
// are the API's own types (api/dto): the use cases answer exactly what the
// API sends.
//
// Some validators and helpers are exported only because handlers that still
// live in api - editing a deployment, scaling, the node and storage actions -
// run the same checks. They move here with those handlers.
package deployment
