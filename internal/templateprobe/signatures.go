package templateprobe

import (
	"regexp"
	"strings"
)

// FailureFamily is the code of a classified launch failure (ADR 0067:
// UPPER_SNAKE). L1 findings that predict the same failure reuse its code.
type FailureFamily string

const (
	FailureArchitecture    FailureFamily = "ARCHITECTURE_NOT_REGISTERED"
	FailureQuantKernel     FailureFamily = "QUANTIZATION_KERNEL_UNAVAILABLE"
	FailureArgv            FailureFamily = "ARGV_REJECTED"
	FailureConfig          FailureFamily = "CONFIG_VALIDATION_FAILURE"
	FailureMissingOperator FailureFamily = "MISSING_OPERATOR"
	FailureDependency      FailureFamily = "DEPENDENCY_CONFLICT"
	// FailureCatalogAsset is a container that cannot open a file the launch
	// contract references - a plugin, a template, a model file. It is a defect
	// in what the catalog declared and what the deployment supplied, not a
	// statement about the model or the engine, and it is the family the
	// nano_v3 plugin defect belongs to.
	FailureCatalogAsset  FailureFamily = "CATALOG_ASSET_MISSING"
	FailureForwardKernel FailureFamily = "FORWARD_KERNEL_FAILURE"
)

type ClassifyOptions struct {
	IncludeForward bool `json:"include_forward,omitempty"`
}

type FailureMatch struct {
	Matched       bool          `json:"matched"`
	Family        FailureFamily `json:"family,omitempty"`
	SignatureID   string        `json:"signature_id,omitempty"`
	Signature     string        `json:"signature,omitempty"`
	Deterministic bool          `json:"deterministic"`
	Excludes      bool          `json:"excludes"`
}

type signatureRule struct {
	family      FailureFamily
	id          string
	signature   string
	pattern     *regexp.Regexp
	forwardOnly bool
}

var deterministicSignatureRules = []signatureRule{
	{FailureArchitecture, "engine.architecture.unregistered", "architecture absent from engine registry", regexp.MustCompile(`(?i)(model architectures?.*(are|is) not supported|unsupported architecture|architecture .* (is )?not registered|failed to inspect the model architecture)`), false},
	{FailureMissingOperator, "runtime.operator.missing", "required runtime operator is absent", regexp.MustCompile(`(?i)(_OpNamespace[^\n]*(has no attribute|object has no attribute)|torch\.ops\.[A-Za-z0-9_.]+[^\n]*(has no attribute|not found))`), false},
	{FailureQuantKernel, "quantization.kernel.unavailable", "no kernel can implement the quantized layer", regexp.MustCompile(`(?i)(failed to find a kernel that can implement|no available kernel.*quant|quantization kernel.*(unavailable|not available|not implemented)|cannot find.*quantization.*kernel)`), false},
	{FailureQuantKernel, "quantization.postprocess.invalid_shape", "quantized weight postprocess produced an invalid tensor shape", regexp.MustCompile(`(?i)(trying to create tensor with negative dimension|negative dimension -?[0-9]+)`), false},
	{FailureArgv, "engine.argv.unrecognized", "engine rejected a command-line argument", regexp.MustCompile(`(?i)(error: unrecognized arguments?:|unrecognized option|unknown argument:)`), false},
	{FailureConfig, "model.config.quantization_mismatch", "model quantization metadata conflicts with the requested method", regexp.MustCompile(`(?i)(quantization method specified in model config.*does not match quantization argument|quantization.*config.*mismatch)`), false},
	{FailureConfig, "model.config.required_attribute_missing", "engine config parser requires an absent model config attribute", regexp.MustCompile(`(?i)([A-Za-z0-9_]*Config['\"]? object has no attribute|object has no attribute ['\"][A-Za-z0-9_]+_config)`), false},
	{FailureConfig, "model.config.validation", "model config validation failed", regexp.MustCompile(`(?i)(validationerror:.*(config|quantization)|invalid task type:)`), false},
	{FailureDependency, "dependency.version_guard", "installed dependency version violates the image contract", regexp.MustCompile(`(?i)(assertionerror: [A-Za-z0-9_.-]+: expected [^\n]+, got [^\n]+|version guard.*fail)`), false},
	{FailureDependency, "dependency.pip_check", "installed packages have incompatible requirements", regexp.MustCompile(`(?i)(requires [^\n]+, but you have [^\n]+(which is incompatible)?|pip check[^\n]*(failed|status[^\n]*[1-9]))`), false},
	{FailureDependency, "dependency.native_build", "required native build dependency is absent", regexp.MustCompile(`(?i)(dependency [\"'][^\"']+[\"'] not found|pkg-config[^\n]*(not found|failed))`), false},
	{FailureDependency, "dependency.runtime_abi", "dependency ABI is incompatible at runtime", regexp.MustCompile(`(?i)(numpy is not available|_ARRAY_API not found|undefined symbol[^\n]*(numpy|torch|python))`), false},
	// A file the launch contract references is absent from the container. The
	// engine reports it while it is still importing and long before it loads a
	// weight, so it is never evidence about the model or the engine: the
	// catalog declared a file that neither the image, the weights repository
	// nor an asset supplied (nano_v3_reasoning_parser.py), or a mount is
	// missing.
	{FailureCatalogAsset, "catalog.asset.file_missing", "the container cannot open a file the launch contract references", regexp.MustCompile(`(?i)(file ?not ?found ?error[^\n]{0,120}no such file or directory|no such file or directory: *['"]|['"][^'"]+['"] *:? *\[errno 2\] *no such file or directory)`), false},
	{FailureForwardKernel, "forward.kernel.vm_fault", "accelerator kernel failed during forward execution", regexp.MustCompile(`(?i)(kernel vmfault|invalid address access.*engine core initialization failed)`), true},
}

// ClassifyFailure recognizes only the deterministic families on the
// whitelist above. An unmatched failure is evidence that the probe did not
// pass, never evidence of incompatibility.
func ClassifyFailure(log string, options ClassifyOptions) FailureMatch {
	log = stripANSI(log)
	for _, rule := range deterministicSignatureRules {
		if rule.forwardOnly && !options.IncludeForward {
			continue
		}
		if rule.pattern.MatchString(log) {
			return FailureMatch{
				Matched: true, Family: rule.family, SignatureID: rule.id, Signature: rule.signature,
				Deterministic: true, Excludes: true,
			}
		}
	}
	return FailureMatch{}
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(value string) string {
	return strings.TrimSpace(ansiPattern.ReplaceAllString(value, ""))
}
