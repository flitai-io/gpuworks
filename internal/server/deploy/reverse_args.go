// reverse_args.go reads an engine argv back into dictionary values. It is the
// inverse of the flag emission in the engine adapters and is driven by the same
// table, so a flag the platform can write is a flag the platform can read.

package deploy

import (
	"strconv"
	"strings"
)

// ReverseArgs separates an already-tokenized engine argv into dictionary values
// and the tokens the dictionary does not describe. Tokens it cannot decode
// stay in the residual verbatim, so nothing an author wrote is ever lost or
// guessed at: an ambiguous flag (one several keys share, such as vLLM's
// --kv-transfer-config) and a value that does not fit its key's type both pass
// straight through.
func ReverseArgs(engine string, args []string) (map[string]any, []string, error) {
	params, _, residual, _, err := reverseArgs(engine, args)
	return params, residual, err
}

// reverseArgs additionally reports which flag carried each key, which is what
// lets a materialized configuration tell the user where a value came from, and
// which tokens it had to pass through even though a single dictionary key owns
// them. That last result is the whole of LintArgs: reading an argv and judging
// one is the same walk, so there is only one, and the importer can never
// disagree with the reader about which flag belongs to which key.
func reverseArgs(engine string, args []string) (map[string]any, map[string]string, []string, []ArgProblem, error) {
	adapter, err := SelectEngine(engine)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	byFlag := flagIndex(engine)
	params := map[string]any{}
	flags := map[string]string{}
	residual := make([]string, 0, len(args))
	var problems []ArgProblem
	for index := 0; index < len(args); index++ {
		token := args[index]
		flag, inlineValue, hasInlineValue := splitInlineFlag(token)
		entries := byFlag[flag]
		if recognized, consumed, ok := recognizeKVBackendToken(adapter, args, index, flag, inlineValue, hasInlineValue); ok {
			// The object a KV offload backend renders is that backend's
			// selection, not an opaque connector (ADR 0069).
			params[paramKVOffloadBackend] = recognized.ID
			flags[paramKVOffloadBackend] = flag
			for key, value := range recognized.Values {
				params[key] = value
				flags[key] = flag
			}
			index += consumed
			continue
		}
		if len(entries) != 1 {
			// No dictionary key owns this flag, or several do and the platform
			// would have to guess which one the author meant.
			residual = append(residual, token)
			continue
		}
		entry := entries[0]
		if entry.spec.Type == "boolean" && !hasInlineValue && (index+1 >= len(args) || strings.HasPrefix(args[index+1], "-")) {
			params[entry.spec.Key] = entry.on
			flags[entry.spec.Key] = flag
			continue
		}
		value := inlineValue
		if !hasInlineValue {
			if index+1 >= len(args) {
				// A value-taking flag with nothing after it. The reader has to
				// pass it through; the importer has to refuse it.
				residual = append(residual, token)
				problems = append(problems, ArgProblem{Flag: flag, Key: entry.spec.Key, Reason: "has no value"})
				continue
			}
			value = args[index+1]
		}
		value = unquoteArgument(value)
		decoded, ok := canonicalValue(entry.spec, value)
		if !ok {
			// The token stays in the residual verbatim and the value token is
			// deliberately not consumed: when the "value" is really the next
			// flag, that flag is still read on its own terms.
			residual = append(residual, token)
			problems = append(problems, ArgProblem{Flag: flag, Value: value, Key: entry.spec.Key, Reason: undecodableReason(entry.spec)})
			continue
		}
		if boolean, isBool := decoded.(bool); isBool && !entry.on {
			// A negative flag written with an explicit value states the negated
			// meaning: --no-enable-prefix-caching=true is "off".
			decoded = !boolean
		}
		params[entry.spec.Key] = decoded
		flags[entry.spec.Key] = flag
		if !hasInlineValue {
			index++
		}
	}
	return params, flags, residual, problems, nil
}

// flagEntry is one flag spelling and the polarity it states. on is meaningless
// for a non-boolean key.
type flagEntry struct {
	spec engineParam
	on   bool
}

func flagIndex(engine string) map[string][]flagEntry {
	params := engineParams(engine)
	index := make(map[string][]flagEntry, len(params)*2)
	for _, spec := range params {
		if spec.Flag != "" {
			index[spec.Flag] = append(index[spec.Flag], flagEntry{spec: spec, on: true})
		}
		if spec.FlagOff != "" {
			index[spec.FlagOff] = append(index[spec.FlagOff], flagEntry{spec: spec, on: false})
		}
	}
	return index
}

func splitInlineFlag(token string) (string, string, bool) {
	if index := strings.IndexByte(token, '='); index > 0 && strings.HasPrefix(token, "-") {
		return token[:index], token[index+1:], true
	}
	return token, "", false
}

func unquoteArgument(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	if value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	return value
}

// recognizeKVBackendToken asks the adapter whether the flag at index carries
// a KV offload backend's own configuration; consumed is how many tokens after
// the flag the value took.
func recognizeKVBackendToken(adapter Engine, args []string, index int, flag, inline string, hasInline bool) (KVBackendRecognition, int, bool) {
	if !strings.HasPrefix(flag, "-") {
		return KVBackendRecognition{}, 0, false
	}
	value, consumed := inline, 0
	if !hasInline {
		if index+1 >= len(args) {
			return KVBackendRecognition{}, 0, false
		}
		value, consumed = args[index+1], 1
	}
	recognized, ok := hooks(adapter).RecognizeKVBackend(flag, value)
	return recognized, consumed, ok
}
