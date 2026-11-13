// Package placement evaluates the deterministic, I/O-free GPU topology
// resolver embedded in an executable inference spec.
package placement

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

const (
	MaxSourceBytes = 32 << 10
	MaxSteps       = 100_000
	// MaxWallTime is a backstop against interpreter-level pathologies, not a
	// latency budget: the step limit above is what deterministically bounds a
	// runaway script, and the dialect forbids arithmetic on locals, so a
	// legitimate resolver finishes in microseconds of CPU. The wall budget must
	// therefore stay generous enough that scheduler starvation cannot fire it —
	// catalog validation compiles every embedded resolver at server startup
	// (one goroutine each, roughly a hundred of them), and on a shared CI
	// runner carrying the full package suite in parallel, goroutines have been
	// measured waiting beyond 2s for a CPU slice, which is what the previous
	// budget turned into spurious "exceeded wall time limit" boot failures
	// across unrelated tests. Any script that legitimately trips 30s of wall
	// time would have blown the step limit long before in CPU terms.
	MaxWallTime          = 30 * time.Second
	MaxGPUs              = 4096
	MaxGPUNameBytes      = 512
	MaxResultVariables   = 64
	MaxResultStringBytes = 4096
	MaxMultiplications   = 8
	// MaxGPUMemoryMiBPerDevice bounds catalog-provided resource contracts well
	// below int64 overflow when converted to bytes. It is deliberately generous:
	// the value is an admission requirement, not an allocation request.
	MaxGPUMemoryMiBPerDevice int64 = 16 * 1024 * 1024
	// MaxPreference bounds the |prefer| an accepting resolver may state. It is
	// an ordering key between accepted candidates, never a quantity.
	MaxPreference int64 = 1_000_000
)

var placementVariableName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

type GPU struct {
	Count        int
	Vendor       string
	Names        []string
	MemoriesGiB  []float64
	MinMemoryGiB float64
	Indexes      []int
	Automatic    bool
}

type Result struct {
	Variables             map[string]any
	GPUMemoryMiBPerDevice int64
	// Prefer is the ordering key a resolver attached to an accepted candidate
	// (accept's prefer argument, default 0). Automatic placement chooses the
	// accepted candidate with the highest Prefer.
	Prefer int64
	Reason string
}

func Compile(source string) error {
	if err := validateProgram(source); err != nil {
		return err
	}
	_, err := execute(context.Background(), source, nil)
	return err
}

func Resolve(ctx context.Context, source string, gpu GPU) (Result, error) {
	if err := validateProgram(source); err != nil {
		return Result{}, err
	}
	if err := validateGPUInput(gpu); err != nil {
		return Result{}, err
	}
	return execute(ctx, source, &gpu)
}

func execute(ctx context.Context, source string, gpu *GPU) (Result, error) {
	if len(source) == 0 || len(source) > MaxSourceBytes {
		return Result{}, fmt.Errorf("placement resolver source must contain 1..%d bytes", MaxSourceBytes)
	}
	thread := &starlark.Thread{Name: "gpuworks-placement"}
	thread.SetMaxExecutionSteps(MaxSteps)
	thread.Print = func(*starlark.Thread, string) {}
	thread.Load = func(*starlark.Thread, string) (starlark.StringDict, error) {
		return nil, errors.New("placement resolver load/import is disabled")
	}
	predeclared := starlark.StringDict{
		"accept": starlark.NewBuiltin("accept", acceptBuiltin),
		"reject": starlark.NewBuiltin("reject", rejectBuiltin),
	}
	type response struct {
		globals starlark.StringDict
		value   starlark.Value
		err     error
	}
	done := make(chan response, 1)
	go func() {
		globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, "placement.star", source, predeclared)
		if err != nil || gpu == nil {
			done <- response{globals: globals, err: err}
			return
		}
		resolver, ok := globals["resolve"]
		if !ok {
			done <- response{err: errors.New("placement resolver must define resolve(gpu)")}
			return
		}
		value, err := starlark.Call(thread, resolver, starlark.Tuple{gpuValue(*gpu)}, nil)
		done <- response{globals: globals, value: value, err: err}
	}()
	timer := time.NewTimer(MaxWallTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		thread.Cancel("context canceled")
		return Result{}, ctx.Err()
	case <-timer.C:
		thread.Cancel("wall time exceeded")
		return Result{}, errors.New("placement resolver exceeded wall time limit")
	case response := <-done:
		if response.err != nil {
			return Result{}, fmt.Errorf("evaluate placement resolver: %w", response.err)
		}
		if _, ok := response.globals["resolve"]; !ok {
			return Result{}, errors.New("placement resolver must define resolve(gpu)")
		}
		if gpu == nil {
			return Result{}, nil
		}
		return decodeResult(response.value)
	}
}

// validateProgram narrows Starlark to a deterministic placement language with
// an explicit allocation bound. Source size and execution steps alone are not
// enough: a single string/list repetition can request enormous memory before
// the step counter advances. Comprehensions, allocation methods, recursion,
// shifts/exponentiation and non-numeric repetition are therefore rejected.
func validateProgram(source string) error {
	if len(source) == 0 || len(source) > MaxSourceBytes {
		return fmt.Errorf("placement resolver source must contain 1..%d bytes", MaxSourceBytes)
	}
	file, err := syntax.LegacyFileOptions().Parse("placement.star", source, 0)
	if err != nil {
		return fmt.Errorf("parse placement resolver: %w", err)
	}
	if len(file.Stmts) != 1 {
		for _, statement := range file.Stmts {
			if _, ok := statement.(*syntax.LoadStmt); ok {
				return errors.New("placement resolver load/import is disabled")
			}
		}
		return errors.New("placement resolver may only define resolve(gpu)")
	}
	definition, ok := file.Stmts[0].(*syntax.DefStmt)
	if !ok || definition.Name == nil || definition.Name.Name != "resolve" || len(definition.Params) != 1 {
		return errors.New("placement resolver may only define resolve(gpu)")
	}
	parameter, ok := definition.Params[0].(*syntax.Ident)
	if !ok || parameter.Name != "gpu" {
		return errors.New("placement resolver must define resolve(gpu)")
	}
	allowedCalls := map[string]struct{}{"accept": {}, "reject": {}, "len": {}, "min": {}, "max": {}, "range": {}}
	var validationErr error
	multiplications := 0
	syntax.Walk(definition, func(node syntax.Node) bool {
		if node == nil || validationErr != nil {
			return false
		}
		switch typed := node.(type) {
		case *syntax.Comprehension:
			validationErr = errors.New("placement resolver comprehensions are disabled by the memory limit")
			return false
		case *syntax.CallExpr:
			identifier, ok := typed.Fn.(*syntax.Ident)
			if !ok {
				validationErr = errors.New("placement resolver method and dynamic calls are disabled")
				return false
			}
			if _, ok := allowedCalls[identifier.Name]; !ok {
				validationErr = fmt.Errorf("placement resolver call to %s is disabled", identifier.Name)
				return false
			}
		case *syntax.BinaryExpr:
			switch typed.Op {
			case syntax.STAR:
				multiplications++
				if multiplications > MaxMultiplications || !boundedNumericExpression(typed.X) || !boundedNumericExpression(typed.Y) {
					validationErr = errors.New("placement resolver permits only bounded numeric multiplication")
					return false
				}
			case syntax.PLUS, syntax.MINUS, syntax.SLASH, syntax.SLASHSLASH, syntax.PERCENT:
				if !boundedNumericExpression(typed.X) || !boundedNumericExpression(typed.Y) {
					validationErr = fmt.Errorf("placement resolver %s is restricted to bounded numeric operands", typed.Op)
					return false
				}
			case syntax.STARSTAR, syntax.LTLT, syntax.GTGT:
				validationErr = fmt.Errorf("placement resolver operator %s is disabled by the memory limit", typed.Op)
				return false
			}
		case *syntax.AssignStmt:
			if typed.Op != syntax.EQ {
				validationErr = errors.New("placement resolver augmented assignment is disabled")
				return false
			}
		}
		return true
	})
	return validationErr
}

func boundedNumericExpression(expression syntax.Expr) bool {
	switch typed := expression.(type) {
	case *syntax.Literal:
		switch value := typed.Value.(type) {
		case int64:
			return value >= -1_000_000_000 && value <= 1_000_000_000
		case float64:
			return !math.IsInf(value, 0) && !math.IsNaN(value) && math.Abs(value) <= 1_000_000_000
		default:
			return false
		}
	case *syntax.DotExpr:
		root, ok := typed.X.(*syntax.Ident)
		if !ok || root.Name != "gpu" || typed.Name == nil {
			return false
		}
		switch typed.Name.Name {
		case "count", "min_memory_gib":
			return true
		}
	case *syntax.IndexExpr:
		field, ok := typed.X.(*syntax.DotExpr)
		if !ok || field.Name == nil || !boundedNumericExpression(typed.Y) {
			return false
		}
		root, ok := field.X.(*syntax.Ident)
		return ok && root.Name == "gpu" && (field.Name.Name == "indexes" || field.Name.Name == "memories_gib")
	case *syntax.ParenExpr:
		return boundedNumericExpression(typed.X)
	case *syntax.UnaryExpr:
		return (typed.Op == syntax.PLUS || typed.Op == syntax.MINUS) && boundedNumericExpression(typed.X)
	case *syntax.BinaryExpr:
		switch typed.Op {
		case syntax.PLUS, syntax.MINUS, syntax.STAR, syntax.SLASH, syntax.SLASHSLASH, syntax.PERCENT:
			return boundedNumericExpression(typed.X) && boundedNumericExpression(typed.Y)
		}
	case *syntax.CallExpr:
		identifier, ok := typed.Fn.(*syntax.Ident)
		return ok && identifier.Name == "len" && len(typed.Args) == 1
	}
	return false
}

func validateGPUInput(gpu GPU) error {
	if gpu.Count < 0 || gpu.Count > MaxGPUs || len(gpu.Indexes) > MaxGPUs || len(gpu.Names) > MaxGPUs || len(gpu.MemoriesGiB) > MaxGPUs {
		return fmt.Errorf("placement GPU input exceeds %d devices", MaxGPUs)
	}
	for _, name := range gpu.Names {
		if len(name) > MaxGPUNameBytes {
			return fmt.Errorf("placement GPU name exceeds %d bytes", MaxGPUNameBytes)
		}
	}
	return nil
}

func rejectBuiltin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var reason string
	if err := starlark.UnpackArgs("reject", args, kwargs, "reason", &reason); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("reject reason cannot be empty")
	}
	return starlarkstruct.FromStringDict(starlark.String("reject"), starlark.StringDict{"reason": starlark.String(reason)}), nil
}

func acceptBuiltin(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var memoryMiB int64
	var variables *starlark.Dict
	var prefer int64
	if err := starlark.UnpackArgs("accept", args, kwargs,
		"gpu_memory_mib_per_device", &memoryMiB,
		"variables?", &variables,
		"prefer?", &prefer,
	); err != nil {
		return nil, err
	}
	if memoryMiB <= 0 || memoryMiB > MaxGPUMemoryMiBPerDevice {
		return nil, fmt.Errorf("gpu_memory_mib_per_device must be within 1..%d", MaxGPUMemoryMiBPerDevice)
	}
	if prefer < -MaxPreference || prefer > MaxPreference {
		return nil, fmt.Errorf("prefer must be within %d..%d", -MaxPreference, MaxPreference)
	}
	if variables == nil {
		variables = starlark.NewDict(0)
	}
	return starlarkstruct.FromStringDict(starlark.String("accept"), starlark.StringDict{
		"gpu_memory_mib_per_device": starlark.MakeInt64(memoryMiB),
		"variables":                 variables,
		"prefer":                    starlark.MakeInt64(prefer),
	}), nil
}

func gpuValue(gpu GPU) starlark.Value {
	names := make([]starlark.Value, len(gpu.Names))
	for index, name := range gpu.Names {
		names[index] = starlark.String(name)
	}
	memories := make([]starlark.Value, len(gpu.MemoriesGiB))
	for index, memory := range gpu.MemoriesGiB {
		memories[index] = starlark.Float(memory)
	}
	indexes := make([]starlark.Value, len(gpu.Indexes))
	for index, value := range gpu.Indexes {
		indexes[index] = starlark.MakeInt(value)
	}
	return starlarkstruct.FromStringDict(starlark.String("gpu"), starlark.StringDict{
		"count": starlark.MakeInt(gpu.Count), "vendor": starlark.String(gpu.Vendor),
		"names": starlark.NewList(names), "memories_gib": starlark.NewList(memories),
		"min_memory_gib": starlark.Float(gpu.MinMemoryGiB), "indexes": starlark.NewList(indexes),
		"automatic": starlark.Bool(gpu.Automatic),
	})
}

func decodeResult(value starlark.Value) (Result, error) {
	if rejected, ok := value.(*starlarkstruct.Struct); ok && rejected.Constructor() == starlark.String("reject") {
		reason, err := rejected.Attr("reason")
		if err != nil {
			return Result{}, err
		}
		return Result{Reason: string(reason.(starlark.String))}, nil
	}
	if accepted, ok := value.(*starlarkstruct.Struct); ok && accepted.Constructor() == starlark.String("accept") {
		memoryValue, err := accepted.Attr("gpu_memory_mib_per_device")
		if err != nil {
			return Result{}, err
		}
		memory, ok := memoryValue.(starlark.Int)
		if !ok {
			return Result{}, errors.New("accept gpu_memory_mib_per_device must be an integer")
		}
		memoryMiB, ok := memory.Int64()
		if !ok || memoryMiB <= 0 || memoryMiB > MaxGPUMemoryMiBPerDevice {
			return Result{}, fmt.Errorf("accept gpu_memory_mib_per_device must be within 1..%d", MaxGPUMemoryMiBPerDevice)
		}
		variablesValue, err := accepted.Attr("variables")
		if err != nil {
			return Result{}, err
		}
		variables, ok := variablesValue.(*starlark.Dict)
		if !ok {
			return Result{}, errors.New("accept variables must be a dictionary")
		}
		decoded, err := decodeVariables(variables)
		if err != nil {
			return Result{}, err
		}
		decoded.GPUMemoryMiBPerDevice = memoryMiB
		preferValue, err := accepted.Attr("prefer")
		if err != nil {
			return Result{}, err
		}
		preferInt, ok := preferValue.(starlark.Int)
		if !ok {
			return Result{}, errors.New("accept prefer must be an integer")
		}
		prefer, ok := preferInt.Int64()
		if !ok || prefer < -MaxPreference || prefer > MaxPreference {
			return Result{}, fmt.Errorf("accept prefer must be within %d..%d", -MaxPreference, MaxPreference)
		}
		decoded.Prefer = prefer
		return decoded, nil
	}
	dictionary, ok := value.(*starlark.Dict)
	if !ok {
		return Result{}, fmt.Errorf("placement resolver must return accept(...), a scalar dictionary, or reject(reason), got %s", value.Type())
	}
	return decodeVariables(dictionary)
}

func decodeVariables(dictionary *starlark.Dict) (Result, error) {
	if dictionary.Len() > MaxResultVariables {
		return Result{}, fmt.Errorf("placement resolver returned %d variables, limit is %d", dictionary.Len(), MaxResultVariables)
	}
	result := Result{Variables: make(map[string]any, dictionary.Len())}
	for _, item := range dictionary.Items() {
		key, ok := item[0].(starlark.String)
		if !ok || !placementVariableName.MatchString(string(key)) {
			return Result{}, errors.New("placement variable names must match [A-Z][A-Z0-9_]*")
		}
		name := string(key)
		if LaunchOwned(name) {
			return Result{}, fmt.Errorf("placement variable %s is stated by the launch, not by a resolver", name)
		}
		switch typed := item[1].(type) {
		case starlark.String:
			if len(typed) > MaxResultStringBytes {
				return Result{}, fmt.Errorf("placement variable %s exceeds %d string bytes", name, MaxResultStringBytes)
			}
			result.Variables[name] = string(typed)
		case starlark.Int:
			value, ok := typed.Int64()
			if !ok {
				return Result{}, fmt.Errorf("placement variable %s exceeds int64", name)
			}
			result.Variables[name] = value
		case starlark.Bool:
			result.Variables[name] = bool(typed)
		default:
			return Result{}, fmt.Errorf("placement variable %s has forbidden %s value", name, typed.Type())
		}
	}
	return result, nil
}
