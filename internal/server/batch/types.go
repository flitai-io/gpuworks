// Package batch is the OpenAI-compatible Batch API: an uploaded JSONL file of
// requests is run asynchronously through the gateway, with the results kept in
// an output file and the failures in an error file (docs/how-to/batch-api.md).
//
// The package knows neither the store nor the gateway: it persists through
// Repo and sends through Sender, which the app implements over them, so the
// executor is tested with a fake clock, a fake sender and an in-memory Repo.
package batch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Statuses of a batch, as the OpenAI Batch API names them.
const (
	StatusValidating = "validating"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
	StatusFinalizing = "finalizing"
	StatusCompleted  = "completed"
	StatusExpired    = "expired"
	StatusCancelling = "cancelling"
	StatusCancelled  = "cancelled"
)

// Statuses are every status of a batch, the ones still moving first, in the
// order a client lists them.
var Statuses = []string{StatusValidating, StatusInProgress, StatusFinalizing, StatusCancelling, StatusCompleted, StatusFailed, StatusExpired, StatusCancelled}

// Terminal reports whether a batch in status has finished for good.
func Terminal(status string) bool {
	switch status {
	case StatusFailed, StatusCompleted, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// Cancellable reports whether Cancel moves a batch in status to cancelling:
// one that has not started or is running. A finalizing batch is assembling its
// results and ends as it is; a cancelling one is already on its way.
func Cancellable(status string) bool {
	return status == StatusValidating || status == StatusInProgress
}

// Purposes of a file: the OpenAI Files API's names.
const (
	PurposeBatch  = "batch"
	PurposeOutput = "batch_output"
)

// Endpoints a batch may call.
const (
	EndpointChat       = "/v1/chat/completions"
	EndpointCompletion = "/v1/completions"
	EndpointEmbedding  = "/v1/embeddings"
)

// The completion windows a batch may ask for, in whole hours: "24h" is OpenAI's
// and the default (CompletionWindow), MinWindowHours to MaxWindowHours are a
// gpuworks extension.
const (
	CompletionWindow = "24h"
	// DefaultWindowHours is CompletionWindow in hours (a test holds them equal).
	DefaultWindowHours = 24
	MinWindowHours     = 1
	MaxWindowHours     = 72
)

// ParseWindow reads a completion window of the form "<hours>h" with whole
// hours from MinWindowHours to MaxWindowHours, written without leading zeros.
func ParseWindow(s string) (time.Duration, bool) {
	hours, found := strings.CutSuffix(s, "h")
	n, err := strconv.Atoi(hours)
	if !found || err != nil || strconv.Itoa(n) != hours || n < MinWindowHours || n > MaxWindowHours {
		return 0, false
	}
	return time.Duration(n) * time.Hour, true
}

// Endpoints is the set of urls an input file's requests may ask for.
var Endpoints = []string{EndpointChat, EndpointCompletion, EndpointEmbedding}

// Limits of an input file and of a batch's metadata (OpenAI's).
const (
	MaxFileBytes     = 200 << 20
	MaxFileLines     = 50000
	MaxLineBytes     = 16 << 20
	MaxMetadataPairs = 16
	MaxMetadataKey   = 64
	MaxMetadataValue = 512
)

// Owner is who a request acts for. A caller sees the files and batches its
// user created; an administrator of the console (All) sees every user's.
type Owner struct {
	UserID   uint
	APIKeyID uint
	All      bool
}

// Sees reports whether the owner may see a row created by userID.
func (o Owner) Sees(userID uint) bool { return o.All || o.UserID == userID }

// File is a stored file's metadata.
type File struct {
	ID        string
	UserID    uint
	APIKeyID  uint
	Purpose   string
	Filename  string
	Bytes     int64
	Lines     int
	Endpoint  string
	CreatedAt time.Time
}

// Batch is a batch's state.
type Batch struct {
	ID               string
	UserID           uint
	APIKeyID         uint
	Endpoint         string
	InputFileID      string
	OutputFileID     string
	ErrorFileID      string
	CompletionWindow string
	// Model is the served name every request is sent to, replacing its line's
	// own body.model; empty when each line names its own.
	Model            string
	Status           string
	ErrorCode        string
	ErrorMessage     string
	Total            int
	Completed        int
	Failed           int
	CheckpointOffset int64
	Metadata         map[string]string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	InProgressAt     *time.Time
	FinalizingAt     *time.Time
	CompletedAt      *time.Time
	FailedAt         *time.Time
	ExpiredAt        *time.Time
	CancellingAt     *time.Time
	CancelledAt      *time.Time
	// Warnings are the non-blocking findings of creating the batch; set on the
	// batch CreateBatch returns, never stored.
	Warnings []Warning
}

// Warning codes of a created batch.
const (
	// WarningModelSleepHeld: the model's deployment sleeps by an operator's or
	// a schedule's order, which a request does not wake; its requests are
	// retried until someone wakes it or the batch expires.
	WarningModelSleepHeld = "model_sleep_held"
	// WarningModelWaits: the model's deployment cannot serve until someone
	// acts (Warning.State: offline, failed or stopped); its requests are
	// retried until it serves or the batch expires.
	WarningModelWaits = "model_waits"
)

// Warning is a finding about a batch that does not refuse it.
type Warning struct {
	Code    string
	Message string
	// Param is the request field the warning is about.
	Param string
	// Models are the served names it is about.
	Models []string
	// State is the state of Models, for WarningModelWaits.
	State ModelState
}

// Owner is the owner a batch acts for when it sends requests.
func (b Batch) Owner() Owner { return Owner{UserID: b.UserID, APIKeyID: b.APIKeyID} }

// Progress is an executor checkpoint: the requests with a result so far and
// the byte length of the partial result file that holds them.
type Progress struct {
	Completed, Failed int
	Offset            int64
}

// Finish is how a batch ends. Output and Error are the files written for it,
// nil for none.
type Finish struct {
	Status       string
	At           time.Time
	Completed    int
	Failed       int
	ErrorCode    string
	ErrorMessage string
	Output       *File
	Error        *File
}

// Errors a Repo reports.
var (
	ErrNotFound  = errors.New("not found")
	ErrFileInUse = errors.New("a batch that has not finished uses the file")
	ErrTerminal  = errors.New("the batch has finished")
)

// Repo persists files and batches. Its writes are per state change or per
// checkpoint, never per request.
type Repo interface {
	CreateFile(f File) error
	GetFile(id string) (File, error)
	ListFiles(owner Owner, purpose, after string, limit int) ([]File, error)
	DeleteFile(id string) error
	CreateBatch(b Batch) error
	GetBatch(id string) (Batch, error)
	ListBatchesAfter(owner Owner, after string, limit int) ([]Batch, error)
	ListBatchesPage(owner Owner, statuses []string, offset, limit int) ([]Batch, int, error)
	// CountByStatus counts the owner's batches in each of the statuses; a
	// status with none is absent.
	CountByStatus(owner Owner, statuses []string) (map[string]int, error)
	Unfinished() ([]Batch, error)
	Start(id string, at time.Time) (bool, error)
	SaveProgress(id string, p Progress) error
	RequestCancel(id string, at time.Time) (Batch, error)
	BeginFinalize(id string, at time.Time) error
	Finish(id string, f Finish) error
	DeleteOutputsBefore(cutoff time.Time) ([]string, error)
}

// ModelState is what a served model name is on the platform, judged from the
// deployments serving it: the best of them when several do.
type ModelState int

const (
	// ModelUnknown: no deployment, running or stopped, serves the name.
	ModelUnknown ModelState = iota
	// ModelRunning: a replica serving it is awake; requests go to it.
	ModelRunning
	// ModelAsleep: none is awake, and one sleeps in a way a request wakes
	// (the gateway holds the request and wakes it).
	ModelAsleep
	// ModelStarting: a replica is being placed, pulled or started; requests
	// are retried until it serves.
	ModelStarting
	// ModelSleepHeld: every replica sleeps by an operator's or a schedule's
	// order, which a request does not wake; requests are retried until
	// someone wakes it.
	ModelSleepHeld
	// ModelOffline: its replicas run on nodes that are not connected;
	// requests are retried until a node returns.
	ModelOffline
	// ModelFailed: its replicas failed to start; requests are retried until
	// someone repairs it.
	ModelFailed
	// ModelStopped: its deployment is stopped; requests are retried until
	// someone starts it.
	ModelStopped
)

// ModelStates are the states a served name can be in, the best for a batch
// first: the order a client lists them in.
var ModelStates = []ModelState{ModelRunning, ModelAsleep, ModelStarting, ModelSleepHeld, ModelOffline, ModelFailed, ModelStopped}

// Known says a deployment serves the name: a batch naming it is accepted,
// and one naming an unknown name refused.
func (m ModelState) Known() bool { return m != ModelUnknown }

// Ready says a request can be served now or can wake the model automatically.
// Other states remain valid asynchronous batch targets when chosen explicitly.
func (m ModelState) Ready() bool {
	return m == ModelRunning || m == ModelAsleep
}

// Waits says requests for a model in this state wait for someone to act (a
// wake, a repair, a start, a node's return) rather than for the platform: a
// batch sent to it is accepted with a warning (WarningModelWaits).
func (m ModelState) Waits() bool {
	return m == ModelSleepHeld || m == ModelOffline || m == ModelFailed || m == ModelStopped
}

// Better says m serves a batch better than other, in the order of
// ModelStates; any state is better than unknown. A name several deployments
// share is as good as the best.
func (m ModelState) Better(other ModelState) bool { return m.rank() < other.rank() }

func (m ModelState) rank() int {
	for i, s := range ModelStates {
		if s == m {
			return i
		}
	}
	return len(ModelStates)
}

// ModelEntry is a served name with what it is to a batch.
type ModelEntry struct {
	Name  string
	State ModelState
	// Endpoints are the batch endpoints (of Endpoints) the deployments
	// serving the name expose, the gateway's own judgement
	// (inferenceapi.AllowsContractRequest); a request for another is refused
	// with 404.
	Endpoints []string
}

// Serves says the name's deployments expose endpoint.
func (e ModelEntry) Serves(endpoint string) bool {
	for _, ep := range e.Endpoints {
		if ep == endpoint {
			return true
		}
	}
	return false
}

// Models looks a served model name up among the platform's deployments, and
// lists every name a batch can be sent to. Both answer from the same rule, so
// the list a client chooses from and the check a create makes agree.
type Models interface {
	// Lookup is the name's entry, ModelUnknown when no deployment serves it.
	Lookup(name string) (ModelEntry, error)
	// List is every served name of a deployment that is not being deleted,
	// none ModelUnknown, in the order of ModelStates and then by name.
	List() ([]ModelEntry, error)
}

// Reply is what sending one request came to.
type Reply struct {
	// Status is the HTTP status, 0 when no response came.
	Status int
	// RequestID is the gateway's id of the request.
	RequestID string
	// Body is the response body.
	Body []byte
	// RetryAfter is the delay the response asked for, zero for none.
	RetryAfter time.Duration
	// TooLarge: the response was larger than the gateway keeps for a reply.
	TooLarge bool
	// Err is why no response came: a transport failure, or the context.
	Err error
}

// Sender sends one request through the gateway as the owner's: the same
// routing, wake hold and accounting as a client's request.
type Sender interface {
	Send(ctx context.Context, owner Owner, path string, body []byte) Reply
}

// Clock is the time the executor reads and waits on.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Error is a refusal carried to the client: an OpenAI-shaped error on /v1, a
// product error on the console's API.
type Error struct {
	Status  int
	Type    string
	Code    string
	Message string
	Param   string
	// Model is the model name a refusal about a model names.
	Model string
	// Endpoint is the batch endpoint a refusal about a model's endpoints
	// names.
	Endpoint string
	// Models are the models of an input file that a refusal about them names,
	// in the order of the file: at most a few dozen, however many it names.
	Models []string
	// Line is the line of the input file the error is about, 0 for none.
	Line int
}

func (e *Error) Error() string { return e.Message }

// Codes of the refusals that callers tell apart: the console maps them to
// product errors of their own.
const (
	CodeFileNotFound         = "file_not_found"
	CodeBatchNotFound        = "batch_not_found"
	CodeModelNotFound        = "model_not_found"
	CodeModelEndpoint        = "model_endpoint_unsupported"
	CodeBatchNotCancellable  = "batch_not_cancellable"
	CodeEmptyFile            = "empty_file"
	CodeTooManyRequests      = "too_many_requests"
	CodeInvalidFileExtension = "invalid_file_extension"
)

// Request fields a refusal or warning about models is about: the model every
// request is sent to, or the input file whose lines name their own.
const (
	ParamModel       = "model"
	ParamInputFileID = "input_file_id"
)

func invalid(code, param, message string) *Error {
	return &Error{Status: 400, Type: "invalid_request_error", Code: code, Param: param, Message: message}
}

func notFound(code, message string) *Error {
	return &Error{Status: 404, Type: "invalid_request_error", Code: code, Message: message}
}
