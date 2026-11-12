// Package serverconfig owns server.yaml, the one file that holds what the
// control plane must know before it can start and what belongs to the machine
// rather than to the cluster: the addresses it binds, where its data and log
// live, and which proxies it trusts. Business settings live in the database;
// facts that can be derived are not stored anywhere.
//
// It is a leaf: `gpuworks install` writes the file and the server reads and
// edits it, so it must not import the server.
package serverconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/storagerules"
)

// FileName is the configuration file kept in the install directory. It is not
// in the data directory on purpose: a database restored onto another machine
// must not bring the old machine's addresses with it.
const FileName = "server.yaml"

// DefaultPort is the port a detected or bare address uses.
const DefaultPort = "8800"

// File is server.yaml.
type File struct {
	// Listen is where this host accepts connections, per function. How others
	// reach it (a reverse proxy, NAT, a domain) is theirs to decide: the
	// server keeps no external address of its own.
	Listen  Listen `yaml:"listen"`
	DataDir string `yaml:"data_dir,omitempty"`
	Log     Log    `yaml:"log,omitempty"`
	// TrustedProxies may supply X-Forwarded-* headers.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty"`
	// TLS protects explicit non-loopback bindings. Local maintenance sockets
	// remain HTTP and are reachable only from this host.
	TLSCertFile string `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile  string `yaml:"tls_key_file,omitempty"`
	// OutboundRestrictPrivateHosts requires explicit exceptions for ordinary private
	// HTTP destinations; false keeps them accessible. Special targets remain
	// forbidden by the outbound transport in either mode.
	OutboundRestrictPrivateHosts bool `yaml:"outbound_restrict_private_hosts,omitempty"`
	// Exceptions when ordinary private HTTP destinations are restricted.
	OutboundAllowPrivateHosts []string `yaml:"outbound_allow_private_hosts,omitempty"`
	// StorageProbeRoots are the roots the startup check compares stored storage
	// paths against. They do not limit new storage paths or probes.
	StorageProbeRoots []string `yaml:"storage_probe_roots,omitempty"`
	// StorageDeniedPaths replaces the built-in list of directories no storage
	// may use. Absent keeps the built-in list; an empty list denies none.
	StorageDeniedPaths *[]string `yaml:"storage_denied_paths,omitempty"`
	// MetricsRetention bounds raw metric and log samples (a Go duration).
	MetricsRetention string `yaml:"metrics_retention,omitempty"`
	// PeerRelay bounds archive forwarding on this server process.
	PeerRelay PeerRelay `yaml:"peer_relay,omitempty"`
	// Management session deadlines (Go durations).
	SessionIdleTimeout string `yaml:"session_idle_timeout,omitempty"`
	SessionMaxLifetime string `yaml:"session_max_lifetime,omitempty"`
	// UdockerBundleSource is a local udocker offline bundle for SSH installs.
	UdockerBundleSource string `yaml:"udocker_bundle_source,omitempty"`
	// SSHProxy is a SOCKS5 proxy (socks5://host:port) SSH to nodes goes
	// through, for sites whose direct egress is too slow.
	SSHProxy string `yaml:"ssh_proxy,omitempty"`
	// UpdateChannel is the distribution channel the installation follows for
	// server upgrades and official catalog updates, nightly or release; empty
	// follows the channel the binary was built for (ADR 0040). The console and
	// `gpuworks upgrade` both read it.
	UpdateChannel string `yaml:"update_channel,omitempty"`
	// AutoCheckUpdates is whether the server checks the channel by itself
	// (when the console opens) and tells it the size and makeup of the fleet
	// while doing so (ADR 0040). Absent means yes; false makes the server ask
	// the channel only when an administrator clicks "check for updates", and
	// then it tells nothing.
	AutoCheckUpdates *bool `yaml:"auto_check_updates,omitempty"`
	// UsageReport is whether the installation takes part in the user
	// experience improvement program: a daily anonymous report of its
	// environment, usage, tasks and errors (docs/reference/usage-report.md),
	// and the fleet's size on automatic update checks. Absent means yes; a
	// private delivery writes false before the first start.
	UsageReport *bool `yaml:"usage_report,omitempty"`
}

// Log configures the server's own log.
type Log struct {
	File    string `yaml:"file,omitempty"`
	MaxSize string `yaml:"max_size,omitempty"`
	Keep    int    `yaml:"keep,omitempty"`
	Level   string `yaml:"level,omitempty"`
}

// Path returns server.yaml inside an install directory.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Load reads server.yaml. Unknown keys are an error: a misspelt key silently
// ignored would leave the server bound somewhere the operator did not intend.
func Load(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	return decodeFile(path, raw)
}

// decodeFile shares strict parsing and validation with readers that already
// hold a file descriptor and must keep its identity tied to the contents.
func decodeFile(path string, raw []byte) (File, error) {
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.Listen.Validate(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if _, err := f.PeerRelay.Policy(); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateUpdateChannel(f.UpdateChannel); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if (f.TLSCertFile == "") != (f.TLSKeyFile == "") {
		return File{}, fmt.Errorf("%s: tls_cert_file and tls_key_file must both be set", path)
	}
	for _, root := range f.StorageProbeRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
			return File{}, fmt.Errorf("%s: storage_probe_roots must contain absolute directories below the filesystem root", path)
		}
	}
	if f.StorageDeniedPaths != nil {
		for _, p := range *f.StorageDeniedPaths {
			if err := storagerules.CheckPathForm(p); err != nil {
				return File{}, fmt.Errorf("%s: storage_denied_paths entry %q: %w", path, p, err)
			}
		}
	}
	return f, nil
}

// ValidateUpdateChannel accepts the published channels and empty.
func ValidateUpdateChannel(channel string) error {
	if channel == "" || distribution.KnownChannel(channel) {
		return nil
	}
	return fmt.Errorf("update_channel %q is not one of %s", channel, strings.Join(distribution.Channels, ", "))
}

// SetUpdateChannel rewrites update_channel in server.yaml, keeping every other
// key and comment; empty removes the key.
func SetUpdateChannel(path, channel string) error {
	if err := ValidateUpdateChannel(channel); err != nil {
		return err
	}
	return editDocument(path, func(root *yaml.Node) error {
		if channel == "" {
			deleteKey(root, "update_channel")
			return nil
		}
		setKey(root, "update_channel", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: channel})
		return nil
	})
}

// SetAutoCheckUpdates rewrites auto_check_updates in server.yaml, keeping every
// other key and comment; on removes the key, which is the default.
func SetAutoCheckUpdates(path string, on bool) error {
	return editDocument(path, func(root *yaml.Node) error {
		if on {
			deleteKey(root, "auto_check_updates")
			return nil
		}
		setKey(root, "auto_check_updates", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		return nil
	})
}

// SetUsageReport rewrites usage_report in server.yaml, keeping every other key
// and comment; on removes the key, which is the default.
func SetUsageReport(path string, on bool) error {
	return editDocument(path, func(root *yaml.Node) error {
		if on {
			deleteKey(root, "usage_report")
			return nil
		}
		setKey(root, "usage_report", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
		return nil
	})
}

// Render produces the file content for a fresh install.
func Render(f File) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# gpuworks control-plane configuration. Restart the server after editing.\n")
	buf.WriteString("# docs/reference/configuration.md describes every key.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write stores a whole file atomically. The installer uses it; the server only
// ever edits single keys through SetListen.
func Write(path string, f File, mode os.FileMode) error {
	payload, err := Render(f)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, payload, mode)
}

// SetListen replaces the listen section in place, keeping every other key
// and the operator's comments.
func SetListen(path string, l Listen) error {
	if err := l.Validate(); err != nil {
		return err
	}
	return editDocument(path, func(root *yaml.Node) error {
		var listen yaml.Node
		if err := listen.Encode(l); err != nil {
			return err
		}
		setKey(root, "listen", &listen)
		return nil
	})
}

// editDocument applies change to the top-level mapping of server.yaml and
// writes the file back atomically with its mode, keeping comments and the
// keys change does not touch.
func editDocument(path string, change func(root *yaml.Node) error) error {
	configDocumentMu.Lock()
	defer configDocumentMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a mapping", path)
	}
	if err := change(doc.Content[0]); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o664)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomic(path, buf.Bytes(), mode)
}

// deleteKey removes key from a mapping node.
func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func setKey(m *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// Keep comments attached to the old value's node.
			value.HeadComment, value.LineComment, value.FootComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment, m.Content[i+1].FootComment
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// writeFileAtomic writes through a temporary neighbour and a rename, so the
// server never starts from a half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
