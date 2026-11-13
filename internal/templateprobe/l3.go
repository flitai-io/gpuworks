package templateprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type L3Task string

const (
	L3TaskText           L3Task = "text"
	L3TaskEmbedding      L3Task = "embedding"
	L3TaskASR            L3Task = "asr"
	L3TaskTTS            L3Task = "tts"
	L3TaskVisionLanguage L3Task = "vision_language"
)

type L3Status string

const (
	L3StatusPass  L3Status = "PASS"
	L3StatusWarn  L3Status = "WARN"
	L3StatusFail  L3Status = "FAIL"
	L3StatusError L3Status = "ERROR"
)

// L3Case is one deterministic semantic acceptance contract. Media paths are
// local to template-probe and are never copied into the result.
type L3Case struct {
	CaseID             string         `json:"case_id"`
	Task               L3Task         `json:"task"`
	Model              string         `json:"model,omitempty"`
	Prompt             string         `json:"prompt,omitempty"`
	ExpectedKeywords   []string       `json:"expected_keywords,omitempty"`
	MaxTokens          int            `json:"max_tokens,omitempty"`
	MaxRepeatedRun     int            `json:"max_repeated_run,omitempty"`
	EmbeddingInputs    []string       `json:"embedding_inputs,omitempty"`
	ExpectedDimensions int            `json:"expected_dimensions,omitempty"`
	MinCosineMargin    float64        `json:"min_cosine_margin,omitempty"`
	AudioPath          string         `json:"audio_path,omitempty"`
	SpeechText         string         `json:"speech_text,omitempty"`
	RequestOverrides   map[string]any `json:"request_overrides,omitempty"`
	ExpectedTranscript string         `json:"expected_transcript,omitempty"`
	MaxWER             float64        `json:"max_wer,omitempty"`
	ASRBaseURL         string         `json:"asr_base_url,omitempty"`
	ASRModel           string         `json:"asr_model,omitempty"`
	ImagePath          string         `json:"image_path,omitempty"`
	ImageMIME          string         `json:"image_mime,omitempty"`
	AcceptanceCriteria []string       `json:"acceptance_criteria,omitempty"`
}

type L3Input struct {
	Deployment     json.RawMessage `json:"deployment"`
	Case           L3Case          `json:"case"`
	KeepDeployment bool            `json:"keep_deployment,omitempty"`
}

type DeploymentHandle struct {
	ID uint `json:"id"`
}

// DeploymentObservation is the minimum evidence the deploy adapter must
// produce before semantic evaluation starts.
type DeploymentObservation struct {
	DeploymentID   uint              `json:"deployment_id"`
	BaseURL        string            `json:"base_url"`
	Model          string            `json:"model"`
	ImageStarted   bool              `json:"image_started"`
	WeightsLoaded  bool              `json:"weights_loaded"`
	Healthy        bool              `json:"healthy"`
	ImageDigest    string            `json:"image_digest,omitempty"`
	EnvironmentMap map[string]string `json:"environment_map,omitempty"`
	EngineArgv     []string          `json:"engine_argv,omitempty"`
}

type TaskEvaluation struct {
	RequestCompleted bool           `json:"request_completed"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	SemanticPassed   bool           `json:"semantic_passed"`
	Warning          string         `json:"warning,omitempty"`
	RequestError     string         `json:"request_error,omitempty"`
	SmokeTest        map[string]any `json:"smoke_test,omitempty"`
	Verification     map[string]any `json:"verification,omitempty"`
}

type L3Deployer interface {
	Create(context.Context, json.RawMessage) (DeploymentHandle, error)
	WaitReady(context.Context, DeploymentHandle) (DeploymentObservation, error)
	Cleanup(context.Context, DeploymentHandle) error
}

type L3Evaluator interface {
	Evaluate(context.Context, DeploymentObservation, L3Case) (TaskEvaluation, error)
}

type L3Verification struct {
	ImageStarted             bool           `json:"image_started"`
	WeightsLoaded            bool           `json:"weights_loaded"`
	Healthy                  bool           `json:"healthy"`
	RequestCompleted         bool           `json:"request_completed"`
	RequestHTTPStatus        int            `json:"request_http_status,omitempty"`
	SemanticOutputValidation string         `json:"semantic_output_validation"`
	FailureStage             string         `json:"failure_stage,omitempty"`
	Error                    string         `json:"error,omitempty"`
	Task                     map[string]any `json:"task,omitempty"`
}

type L3Cleanup struct {
	Attempted bool   `json:"attempted"`
	Completed bool   `json:"completed"`
	Retained  bool   `json:"retained"`
	Error     string `json:"error,omitempty"`
}

// L3Result retains the field names proven by hygon-live-validation.json while
// making the v4 platform/semantic split explicit.
type L3Result struct {
	CaseID             string            `json:"case_id"`
	Status             L3Status          `json:"status"`
	TestedAt           time.Time         `json:"tested_at"`
	DeploymentID       uint              `json:"deployment_id,omitempty"`
	PlatformCompatible bool              `json:"platform_compatible"`
	ImageDigest        string            `json:"image_digest,omitempty"`
	EnvironmentMap     map[string]string `json:"environment_map"`
	EngineArgv         []string          `json:"engine_argv"`
	SmokeTest          map[string]any    `json:"smoke_test"`
	AcceptanceCriteria []string          `json:"acceptance_criteria"`
	Verification       L3Verification    `json:"verification"`
	Warnings           []string          `json:"warnings,omitempty"`
	Cleanup            L3Cleanup         `json:"cleanup"`
}

type PlatformStageError struct {
	Stage string
	Err   error
}

func (e PlatformStageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e PlatformStageError) Unwrap() error { return e.Err }

type HarnessStageError struct {
	Stage string
	Err   error
}

func (e HarnessStageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e HarnessStageError) Unwrap() error { return e.Err }

type L3Runner struct {
	Deployer       L3Deployer
	Evaluator      L3Evaluator
	CleanupTimeout time.Duration
	Now            func() time.Time
}

func (r L3Runner) Run(ctx context.Context, input L3Input) (L3Result, error) {
	if r.Deployer == nil || r.Evaluator == nil {
		return L3Result{}, errors.New("L3 deployer and evaluator are required")
	}
	if len(input.Deployment) == 0 || !json.Valid(input.Deployment) {
		return L3Result{}, errors.New("deployment must be a valid JSON object")
	}
	if err := ValidateL3Case(input.Case); err != nil {
		return L3Result{}, err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	result := L3Result{
		CaseID: input.Case.CaseID, Status: L3StatusError, TestedAt: now().UTC(),
		EnvironmentMap: map[string]string{}, EngineArgv: []string{}, SmokeTest: map[string]any{},
		AcceptanceCriteria: acceptanceCriteria(input.Case),
		Verification:       L3Verification{SemanticOutputValidation: "NOT_RUN"},
	}
	handle, err := r.Deployer.Create(ctx, input.Deployment)
	if err != nil {
		applyL3Error(&result, HarnessStageError{Stage: "deployment_create", Err: err})
		return result, nil //nolint:nilerr // Deployment failure is recorded in the L3 result with its failure stage.
	}
	result.DeploymentID = handle.ID
	if input.KeepDeployment {
		result.Cleanup.Retained = true
	}

	observation, waitErr := r.Deployer.WaitReady(ctx, handle)
	if waitErr != nil {
		applyL3Error(&result, waitErr)
		r.finishCleanup(&result, handle, input.KeepDeployment)
		return result, nil
	}
	copyObservation(&result, observation)
	if !observation.ImageStarted || !observation.WeightsLoaded || !observation.Healthy {
		applyL3Error(&result, PlatformStageError{Stage: "deployment_ready", Err: errors.New("deployment adapter returned incomplete platform gates")})
		r.finishCleanup(&result, handle, input.KeepDeployment)
		return result, nil
	}

	evaluation, evalErr := r.Evaluator.Evaluate(ctx, observation, input.Case)
	if evalErr != nil {
		applyL3Error(&result, evalErr)
		r.finishCleanup(&result, handle, input.KeepDeployment)
		return result, nil
	}
	result.SmokeTest = evaluation.SmokeTest
	if result.SmokeTest == nil {
		result.SmokeTest = map[string]any{}
	}
	result.Verification.RequestCompleted = evaluation.RequestCompleted
	result.Verification.RequestHTTPStatus = evaluation.HTTPStatus
	result.Verification.Task = evaluation.Verification
	if !evaluation.RequestCompleted {
		if errors.Is(ctx.Err(), context.Canceled) {
			applyL3Error(&result, HarnessStageError{Stage: "canceled", Err: ctx.Err()})
		} else {
			errText := strings.TrimSpace(evaluation.RequestError)
			if errText == "" {
				errText = "task request did not complete successfully"
			}
			applyL3Error(&result, PlatformStageError{Stage: "task_request", Err: errors.New(errText)})
		}
	} else {
		result.PlatformCompatible = true
		if evaluation.SemanticPassed {
			result.Status = L3StatusPass
			result.Verification.SemanticOutputValidation = "PASS"
		} else {
			result.Status = L3StatusWarn
			result.Verification.SemanticOutputValidation = "WARN"
			warning := strings.TrimSpace(evaluation.Warning)
			if warning == "" {
				warning = "semantic output validation failed"
			}
			result.Warnings = append(result.Warnings, warning)
		}
	}
	r.finishCleanup(&result, handle, input.KeepDeployment)
	return result, nil
}

func (r L3Runner) finishCleanup(result *L3Result, handle DeploymentHandle, keep bool) {
	if keep {
		return
	}
	result.Cleanup.Attempted = true
	timeout := r.CleanupTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := r.Deployer.Cleanup(cleanupCtx, handle); err != nil {
		result.Cleanup.Error = err.Error()
		result.Warnings = append(result.Warnings, "temporary deployment cleanup failed: "+err.Error())
		result.Status = L3StatusError
		return
	}
	result.Cleanup.Completed = true
}

func copyObservation(result *L3Result, observation DeploymentObservation) {
	result.DeploymentID = observation.DeploymentID
	result.ImageDigest = observation.ImageDigest
	result.EnvironmentMap = observation.EnvironmentMap
	if result.EnvironmentMap == nil {
		result.EnvironmentMap = map[string]string{}
	}
	result.EngineArgv = observation.EngineArgv
	if result.EngineArgv == nil {
		result.EngineArgv = []string{}
	}
	result.Verification.ImageStarted = observation.ImageStarted
	result.Verification.WeightsLoaded = observation.WeightsLoaded
	result.Verification.Healthy = observation.Healthy
}

func applyL3Error(result *L3Result, err error) {
	var platform PlatformStageError
	if errors.As(err, &platform) {
		result.Status = L3StatusFail
		result.Verification.FailureStage = platform.Stage
		result.Verification.Error = platform.Err.Error()
		return
	}
	var harness HarnessStageError
	if errors.As(err, &harness) {
		result.Status = L3StatusError
		result.Verification.FailureStage = harness.Stage
		result.Verification.Error = harness.Err.Error()
		return
	}
	result.Status = L3StatusError
	result.Verification.FailureStage = "harness"
	result.Verification.Error = fmt.Sprint(err)
}

func acceptanceCriteria(testCase L3Case) []string {
	if len(testCase.AcceptanceCriteria) > 0 {
		return append([]string(nil), testCase.AcceptanceCriteria...)
	}
	common := []string{"image starts", "real model weights load", "service becomes healthy", "task request returns HTTP 2xx"}
	return append(common, semanticCriterion(testCase.Task))
}

func semanticCriterion(task L3Task) string {
	switch task {
	case L3TaskText:
		return "assistant output is nonempty, non-repetitive, and contains the expected keyword"
	case L3TaskEmbedding:
		return "finite nonzero embeddings have the expected dimension and semantic similarity ordering"
	case L3TaskASR:
		return "transcription word error rate is within the configured threshold"
	case L3TaskTTS:
		return "speech output is a valid WAV and ASR backread word error rate is within threshold"
	case L3TaskVisionLanguage:
		return "vision-language response contains the normalized expected keyword"
	default:
		return "semantic output validation passes"
	}
}
