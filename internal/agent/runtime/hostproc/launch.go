package hostproc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	oci "gpuworks.ai/gpuworks/internal/agent/runtime/udocker/native"
)

// A workload's spec is written for a container: its argv and environment name
// paths inside the container's filesystem, made of the image and the bind
// mounts. A host process has no such filesystem, so the launch translates each
// of those paths to where the content really is on the host:
//
//   - a path under a mount target (the model at /model) becomes the same path
//     under the mount's source;
//   - an absolute path that exists in the environment package ("/venv/bin/vllm")
//     becomes that path under the package's unpacked root, exactly as it would
//     resolve inside a container built from the package;
//   - anything else is a host path and is left alone.
//
// Only whole tokens are translated, plus the value of a "--flag=value" or
// "KEY=value" token and each element of a colon-separated path list, because
// those are the places a launch puts a path. The value of a flag in nameFlags
// is a name, not a path, and is kept as written. A shell script (an argument with
// whitespace) gets its mount targets translated where they stand as words: a
// pre-start script names the model mount that way. A manual command is such a
// script too, so there the value of a nameFlags flag is likewise kept.
type pathMap struct {
	root   string          // unpacked environment package
	mounts []runtime.Mount // longest target first
	exists func(string) bool
}

func newPathMap(root string, mounts []runtime.Mount, exists func(string) bool) pathMap {
	sorted := append([]runtime.Mount(nil), mounts...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Target) > len(sorted[j].Target) })
	return pathMap{root: root, mounts: sorted, exists: exists}
}

// path translates one absolute container path; ok is false for a host path.
func (m pathMap) path(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") {
		return p, false
	}
	clean := filepath.Clean(p)
	for _, mount := range m.mounts {
		target := filepath.Clean(mount.Target)
		if target == "/" || target == "" {
			continue
		}
		if clean == target {
			return keepTrailingSlash(p, mount.Source), true
		}
		if rest, ok := strings.CutPrefix(clean, target+"/"); ok {
			return keepTrailingSlash(p, filepath.Join(mount.Source, rest)), true
		}
	}
	if m.root != "" && clean != "/" && m.exists(filepath.Join(m.root, clean)) {
		return keepTrailingSlash(p, filepath.Join(m.root, clean)), true
	}
	return p, false
}

func keepTrailingSlash(original, mapped string) string {
	if strings.HasSuffix(original, "/") && !strings.HasSuffix(mapped, "/") {
		return mapped + "/"
	}
	return mapped
}

// value translates a value that may be one path or a colon-separated list of
// absolute paths.
func (m pathMap) value(v string) string {
	if strings.Contains(v, ":") {
		parts := strings.Split(v, ":")
		all := true
		for _, part := range parts {
			if !strings.HasPrefix(part, "/") {
				all = false
				break
			}
		}
		if all {
			for i, part := range parts {
				parts[i], _ = m.path(part)
			}
			return strings.Join(parts, ":")
		}
	}
	mapped, _ := m.path(v)
	return mapped
}

// flagOrKey matches the "--flag=" or "KEY=" prefix of a token whose value is
// to be translated.
var flagOrKey = regexp.MustCompile(`^(-{1,2}[A-Za-z0-9][A-Za-z0-9._-]*|[A-Za-z_][A-Za-z0-9_]*)=`)

// nameFlags are the flags whose value is a name that merely looks like a path.
// A launch with no model alias names the model after its mount target
// ("--served-model-name /model"), and that is the name the gateway asks for:
// translating it would leave the engine answering under the host path.
var nameFlags = []string{"--served-model-name"}

// isNameFlag reports whether tok is a flag in nameFlags.
func isNameFlag(tok string) bool {
	return slices.Contains(nameFlags, tok)
}

// scriptNameValue matches a nameFlags flag in a shell script and captures its
// value: after "=" or blanks (line continuations included), bare or quoted.
// The flag itself may be quoted ("--served-model-name" /model), since the
// server's deploy.ParseArgsText reads that as the same flag and expects the
// engine under the name that follows.
var scriptNameValue = func() *regexp.Regexp {
	flags := make([]string, len(nameFlags))
	for i, flag := range nameFlags {
		flags[i] = regexp.QuoteMeta(flag)
	}
	return regexp.MustCompile(`(?:^|[\s"'(;&|])(?:` + strings.Join(flags, "|") + `)["']?(?:=|(?:[ \t]|\\\n)+)("[^"]*"|'[^']*'|[^\s"';)&|<>]+)`)
}()

// args translates a whole argv, keeping the values of nameFlags.
func (m pathMap) args(argv []string) []string {
	out := make([]string, len(argv))
	for i, tok := range argv {
		if i > 0 && isNameFlag(argv[i-1]) {
			out[i] = tok
			continue
		}
		if flag, _, ok := strings.Cut(tok, "="); ok && isNameFlag(flag) {
			out[i] = tok
			continue
		}
		out[i] = m.arg(tok)
	}
	return out
}

// arg translates one argv token.
func (m pathMap) arg(tok string) string {
	if strings.ContainsAny(tok, " \t\n") {
		return m.script(tok)
	}
	if loc := flagOrKey.FindStringIndex(tok); loc != nil {
		return tok[:loc[1]] + m.value(tok[loc[1]:])
	}
	return m.value(tok)
}

// script translates the mount targets that stand as words in a shell script.
// It is one pass over the script, longest target first at each word, so a
// translated path is never translated again by a shorter target, and the
// character that ends one word can start the next ("/model /model").
func (m pathMap) script(s string) string {
	var targets []string
	sources := map[string]string{}
	for _, mount := range m.mounts {
		target := filepath.Clean(mount.Target)
		if target == "/" || target == "" {
			continue
		}
		if _, dup := sources[target]; !dup {
			targets = append(targets, regexp.QuoteMeta(target))
			sources[target] = mount.Source
		}
	}
	if len(targets) == 0 {
		return s
	}
	word := regexp.MustCompile(`(?:^|[` + regexp.QuoteMeta(wordStart) + `])(` + strings.Join(targets, "|") + `)`)
	names := scriptNameValue.FindAllStringSubmatchIndex(s, -1)
	isName := func(at int) bool {
		for _, loc := range names {
			if at >= loc[2] && at < loc[3] {
				return true
			}
		}
		return false
	}
	var out strings.Builder
	last := 0
	for _, loc := range word.FindAllStringSubmatchIndex(s, -1) {
		start, end := loc[2], loc[3]
		if isName(start) {
			continue // a name, kept as written (nameFlags)
		}
		if !endsWord(s, end) {
			continue // a longer word that only begins with the target
		}
		out.WriteString(s[last:start])
		out.WriteString(sources[s[start:end]])
		last = end
	}
	out.WriteString(s[last:])
	return out.String()
}

// wordStart and wordEnd are the characters that may stand right before and
// right after a mount target for it to be a word of a shell script: blanks,
// quotes and shell operators, "=" or ":" before a value ("--x=/model",
// "PATH=/a:/model"), and "/" after it for a path below the target.
const (
	wordStart = " \t\n\"'=:(;&|<>"
	wordEnd   = "/ \t\n\"');&|<>"
)

// endsWord reports whether a word of s that runs up to s[end] ends there: at
// the end of s, at a wordEnd character or at a line continuation.
func endsWord(s string, end int) bool {
	if end >= len(s) || strings.IndexByte(wordEnd, s[end]) >= 0 {
		return true
	}
	return strings.HasPrefix(s[end:], "\\\n")
}

// instanceDirs are the per-instance and per-environment directories a host
// process gets instead of the container's private filesystem.
type instanceDirs struct {
	Home  string // the workload's HOME, removed with the instance
	Tmp   string // TMPDIR, removed with the instance
	Cache string // caches shared by every instance of one environment package
}

// launch is a resolved host process.
type launch struct {
	Path string
	Args []string // argv, Args[0] included
	Env  []string
	Dir  string
}

// hostPATH is the system part of every workload's PATH. The agent's own PATH
// is deliberately not inherited: on a developer's Mac it leads to Homebrew and
// pyenv, whose libraries and interpreters must not leak into an engine
// environment that pins its own.
const hostPATH = "/usr/bin:/bin:/usr/sbin:/sbin"

// hostEnvKeys are the variables a host process inherits from the agent: who
// it runs as, its locale, and the proxy the operator configured.
var hostEnvKeys = []string{
	"USER", "LOGNAME", "SHELL", "LANG", "LC_ALL",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "ALL_PROXY", "all_proxy",
}

// planLaunch resolves a workload's container-shaped spec into a host process
// with docker's entrypoint semantics: the spec's entrypoint replaces the
// image's, and the image's Cmd only applies when neither the entrypoint nor
// the command was given.
func planLaunch(root string, config oci.ImageConfig, spec runtime.WorkloadSpec, dirs instanceDirs, getenv func(string) string, exists func(string) bool) (launch, error) {
	paths := newPathMap(root, spec.Mounts, exists)

	argv := append([]string(nil), config.Entrypoint...)
	cmd := config.Cmd
	if len(spec.Entrypoint) > 0 {
		argv = append([]string(nil), spec.Entrypoint...)
		cmd = nil
	}
	if len(spec.Cmd) > 0 {
		cmd = spec.Cmd
	}
	argv = append(argv, cmd...)
	if len(argv) == 0 {
		return launch{}, fmt.Errorf("workload %s has no command: neither the spec nor image %s names one", spec.Name, spec.Image)
	}
	argv = paths.args(argv)

	env := map[string]string{}
	for _, key := range hostEnvKeys {
		if v := getenv(key); v != "" {
			env[key] = v
		}
	}
	if env["LANG"] == "" {
		env["LANG"] = "en_US.UTF-8"
	}
	imagePATH := ""
	for _, entry := range config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if key == "PATH" {
			imagePATH = value
			continue
		}
		env[key] = paths.value(value)
	}
	for key, value := range spec.Env {
		if key == "PATH" {
			imagePATH = value
			continue
		}
		env[key] = paths.arg(value)
	}
	for key := range env {
		// The macOS dynamic loader honours these, and a value from the
		// agent's environment or a Linux-minded template would redirect the
		// engine's libraries to the host's.
		if strings.HasPrefix(key, "DYLD_") || key == "LD_LIBRARY_PATH" || key == "LD_PRELOAD" {
			delete(env, key)
		}
	}
	env["HOME"] = dirs.Home
	env["TMPDIR"] = dirs.Tmp
	setDefault(env, "XDG_CACHE_HOME", dirs.Cache)
	// torch's compile cache lives under the temp dir by default, which is
	// per instance; keep it with the environment's other caches so a restart
	// does not recompile.
	setDefault(env, "TORCHINDUCTOR_CACHE_DIR", filepath.Join(dirs.Cache, "torchinductor"))
	env["PATH"] = workloadPATH(paths, imagePATH)

	resolved, err := resolveExecutable(argv[0], env["PATH"], exists)
	if err != nil {
		return launch{}, fmt.Errorf("workload %s: %w", spec.Name, err)
	}

	dir := dirs.Home
	if config.WorkingDir != "" {
		if mapped, ok := paths.path(config.WorkingDir); ok {
			dir = mapped
		}
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	envList := make([]string, 0, len(keys))
	for _, key := range keys {
		envList = append(envList, key+"="+env[key])
	}
	return launch{Path: resolved, Args: argv, Env: envList, Dir: dir}, nil
}

func setDefault(env map[string]string, key, value string) {
	if env[key] == "" {
		env[key] = value
	}
}

// workloadPATH is the image's PATH with its package directories translated,
// then the system directories it does not already name.
func workloadPATH(paths pathMap, imagePATH string) string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range strings.Split(imagePATH, ":") {
		if mapped, ok := paths.path(dir); ok {
			add(mapped)
		}
	}
	for _, dir := range strings.Split(hostPATH, ":") {
		add(dir)
	}
	return strings.Join(dirs, ":")
}

// resolveExecutable finds argv[0] the way execvp would with the workload's
// PATH; the agent's own PATH plays no part.
func resolveExecutable(name, path string, exists func(string) bool) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range strings.Split(path, ":") {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if exists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in the workload PATH %s", name, path)
}

// pathExists is the exists check of a real launch: an entry, of any type,
// whether or not a symlink resolves.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
