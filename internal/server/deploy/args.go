package deploy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// builder accumulates argv while honoring "a flag already spoken for is never
// re-injected". Two things speak for a flag: a residual token the caller wrote
// (which is why user residual still beats platform injection) and an earlier
// injection by the builder itself (which is why a quirk cannot overwrite the
// value the deployment's own configuration produced).
type builder struct {
	// engine is which adapter is building, and therefore which column of the
	// parameter dictionary a key's flag is read from. Carrying it here is what
	// lets an adapter inject a dictionary parameter without naming its flag.
	engine  string
	blocked map[string]bool
	argv    []string
	params  map[string]engineParam
}

func newBuilder(engine string, blocked map[string]bool) *builder {
	if blocked == nil {
		blocked = map[string]bool{}
	}
	return &builder{engine: engine, blocked: blocked}
}

// injectKV adds "flag value" unless the flag is already spoken for.
func (b *builder) injectKV(flag, value string) {
	if flag == "" || b.blocked[flag] {
		return
	}
	b.blocked[flag] = true
	b.argv = append(b.argv, flag, value)
}

// injectFlag adds a bare "flag" unless it is already spoken for. An empty flag
// is how the dictionary says "this engine spells this polarity by saying
// nothing", so it emits nothing.
func (b *builder) injectFlag(flag string) {
	if flag == "" || b.blocked[flag] {
		return
	}
	b.blocked[flag] = true
	b.argv = append(b.argv, flag)
}

func (b *builder) args() []string { return b.argv }

// injectDictionary writes one dictionary parameter: the flag spelling and the
// value format come from the row this engine has for the key. The adapter still
// decides when a stated value becomes a flag, and in which order — those are
// the command's own semantics — but it never spells the flag itself, so a
// spelling that moves in the table moves here too.
//
// A boolean takes the polarity the dictionary spells, and an empty one is the
// table saying "this engine says nothing for that polarity". A key this engine
// has no flag for, a platform parameter, and a value the row cannot format all
// emit nothing rather than a token nobody can attribute to a parameter.
func (b *builder) injectDictionary(key string, value any) {
	spec, known := b.dictionary()[key]
	if !known || spec.Platform {
		return
	}
	switch spec.Type {
	case "boolean":
		stated, ok := canonicalValue(spec, value)
		if !ok {
			return
		}
		truth, _ := stated.(bool)
		flag := spec.Flag
		if !truth {
			flag = spec.FlagOff
		}
		b.injectFlag(flag)
	case "integer":
		stated, ok := canonicalValue(spec, value)
		if !ok {
			return
		}
		number, _ := stated.(int64)
		b.injectKV(spec.Flag, strconv.FormatInt(number, 10))
	case "number":
		number, ok := toFloat(value)
		if !ok {
			return
		}
		b.injectKV(spec.Flag, strconv.FormatFloat(number, 'g', -1, 64))
	case "json":
		b.injectKV(spec.Flag, jsonText(value))
	default:
		// A configuration stored before the dictionary changed an engine's
		// spelling (SGLang's eagle3 -> EAGLE3) still reaches the engine in the
		// spelling it accepts, for an enum and for a free-text key's suggested
		// values alike. A value the row does not know goes out as written, for
		// the engine to judge.
		if stated, ok := canonicalValue(spec, value); ok {
			value = stated
		}
		b.injectKV(spec.Flag, fmt.Sprint(value))
	}
}

// injectDictionaryBool writes a tri-state dictionary boolean. A nil value is
// how a configuration says nobody stated it, so the engine keeps its own
// default and no flag comes out.
func (b *builder) injectDictionaryBool(key string, value *bool) {
	if value == nil {
		return
	}
	b.injectDictionary(key, *value)
}

// dictionary resolves the parameter rows once per builder: the table is fixed
// after init, and one Build injects dozens of parameters through it.
func (b *builder) dictionary() map[string]engineParam {
	if b.params == nil {
		b.params = engineParams(b.engine)
	}
	return b.params
}

// flagSet extracts the set of flag names present in a user argv slice, handling
// both "--k=v" and "--k v" and bare "--flag" forms.
func flagSet(args []string) map[string]bool {
	set := make(map[string]bool)
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue // a value belonging to the previous flag
		}
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		set[name] = true
	}
	return set
}

func filterArgLayer(args []string, blocked map[string]bool) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		token := args[index]
		name := token
		if at := strings.IndexByte(name, '='); at > 0 {
			name = name[:at]
		}
		if !blocked[name] {
			out = append(out, token)
			continue
		}
		if strings.Contains(token, "=") || index+1 >= len(args) {
			continue
		}
		// Imported passthrough flags are tokenized as flag/value pairs. A
		// following flag means this one was a boolean switch.
		if !strings.HasPrefix(args[index+1], "-") || looksLikeNegativeNumber(args[index+1]) {
			index++
		}
	}
	return out
}

// residualLayers prepares the two passthrough layers for one build. Only
// tokens the parameter dictionary does not describe reach here, so the rules
// are the two the launch contract states: a flag the user wrote is never
// injected by the platform and never re-supplied by the template, and a flag
// the template wrote is not injected by the platform either.
func residualLayers(templateArgs, userArgs []string) (blockers map[string]bool, template []string) {
	userFlags := flagSet(userArgs)
	template = filterArgLayer(templateArgs, userFlags)
	blockers = make(map[string]bool, len(userFlags)+len(template))
	for flag := range userFlags {
		blockers[flag] = true
	}
	for flag := range flagSet(template) {
		blockers[flag] = true
	}
	return blockers, template
}

func looksLikeNegativeNumber(value string) bool {
	if len(value) < 2 || value[0] != '-' {
		return false
	}
	for _, char := range value[1:] {
		if (char < '0' || char > '9') && char != '.' {
			return false
		}
	}
	return true
}

// ParseArgsText parses the expert text escape hatch without flattening quoted
// JSON or values containing spaces. A JSON string array is accepted directly.
func ParseArgsText(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}
	if strings.HasPrefix(input, "[") {
		var args []string
		if err := json.Unmarshal([]byte(input), &args); err != nil {
			return nil, fmt.Errorf("parse argument JSON array: %w", err)
		}
		return args, nil
	}
	var (
		args     []string
		current  strings.Builder
		quote    rune
		escaped  bool
		hasToken bool
	)
	flush := func() {
		if hasToken {
			args = append(args, current.String())
			current.Reset()
			hasToken = false
		}
	}
	for _, char := range input {
		if escaped {
			current.WriteRune(char)
			hasToken = true
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			hasToken = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			hasToken = true
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			hasToken = true
			continue
		}
		if unicode.IsSpace(char) {
			flush()
			continue
		}
		current.WriteRune(char)
		hasToken = true
	}
	if escaped {
		return nil, fmt.Errorf("argument text ends with an escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("argument text has an unterminated quote")
	}
	flush()
	return args, nil
}

// FormatArgsText renders argv tokens back into the expert text box format
// ParseArgsText reads. A token that would not survive the round trip is single
// quoted, which is the same quoting the parser accepts.
func FormatArgsText(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, quoteArgument(arg))
	}
	return strings.Join(parts, " ")
}

func quoteArgument(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n\r'\"\\") {
		return arg
	}
	// Close the quoted run, emit an escaped quote, reopen: the parser reads a
	// backslash outside quotes as an escape, so this survives the round trip.
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}
