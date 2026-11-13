package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Spec is a launchd job as its plist states it: the part gpuworks writes for
// the agent (Job) and for the control plane alike. Every job is kept alive
// (KeepAlive, restarted 3 s after it exits) and started when loaded.
type Spec struct {
	Label string
	// Program, when present, overrides the executable in ProgramArguments.
	Program string
	// User is the account a launch daemon runs as; empty runs a daemon as
	// root and a launch agent as the owner of its session.
	User       string
	Args       []string
	Env        map[string]string
	WorkingDir string
	// StderrPath takes what the process writes to stderr; empty discards it.
	StderrPath string
	// AbandonProcessGroup leaves the job's children running when it stops;
	// otherwise launchd stops them with it.
	AbandonProcessGroup bool
	// ExitTimeout is how long launchd waits after SIGTERM before SIGKILL, in
	// seconds; zero keeps launchd's default.
	ExitTimeout int
}

// Plist renders the spec. A flag and its value share a line.
func (s Spec) Plist() []byte {
	var b bytes.Buffer
	str := func(v string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(v))
		return "<string>" + e.String() + "</string>"
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "  <key>Label</key>%s\n", str(s.Label))
	if s.Program != "" {
		fmt.Fprintf(&b, "  <key>Program</key>%s\n", str(s.Program))
	}
	if s.User != "" {
		fmt.Fprintf(&b, "  <key>UserName</key>%s\n", str(s.User))
	}
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for i := 0; i < len(s.Args); i++ {
		line := str(s.Args[i])
		if strings.HasPrefix(s.Args[i], "--") && i+1 < len(s.Args) && !strings.HasPrefix(s.Args[i+1], "--") {
			i++
			line += str(s.Args[i])
		}
		fmt.Fprintf(&b, "    %s\n", line)
	}
	b.WriteString("  </array>\n")
	if len(s.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    <key>%s</key>%s\n", k, str(s.Env[k]))
		}
		b.WriteString("  </dict>\n")
	}
	if s.WorkingDir != "" {
		fmt.Fprintf(&b, "  <key>WorkingDirectory</key>%s\n", str(s.WorkingDir))
	}
	b.WriteString(`  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>3</integer>
`)
	if s.AbandonProcessGroup {
		b.WriteString("  <key>AbandonProcessGroup</key><true/>\n")
	}
	if s.ExitTimeout > 0 {
		fmt.Fprintf(&b, "  <key>ExitTimeOut</key><integer>%d</integer>\n", s.ExitTimeout)
	}
	b.WriteString(`  <key>SoftResourceLimits</key><dict><key>NumberOfFiles</key><integer>65536</integer></dict>
  <key>HardResourceLimits</key><dict><key>NumberOfFiles</key><integer>65536</integer></dict>
  <key>StandardOutPath</key><string>/dev/null</string>
`)
	stderr := s.StderrPath
	if stderr == "" {
		stderr = "/dev/null"
	}
	fmt.Fprintf(&b, "  <key>StandardErrorPath</key>%s\n", str(stderr))
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// ReadSpec reads the label, user, arguments and environment of a plist.
func ReadSpec(data []byte) (Spec, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var spec Spec
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Spec{}, fmt.Errorf("read launchd plist: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := dec.DecodeElement(&key, &start); err != nil {
			return Spec{}, fmt.Errorf("read launchd plist: %w", err)
		}
		switch key {
		case "Label":
			spec.Label, err = nextString(dec)
		case "Program":
			spec.Program, err = nextString(dec)
		case "UserName":
			spec.User, err = nextString(dec)
		case "ProgramArguments":
			spec.Args, err = stringArray(dec)
		case "EnvironmentVariables":
			spec.Env, err = stringDict(dec)
		case "WorkingDirectory":
			spec.WorkingDir, err = nextString(dec)
		case "StandardErrorPath":
			spec.StderrPath, err = nextString(dec)
		}
		if err != nil {
			return Spec{}, fmt.Errorf("read launchd plist %s: %w", key, err)
		}
	}
	if len(spec.Args) == 0 {
		return Spec{}, fmt.Errorf("read launchd plist: no ProgramArguments")
	}
	return spec, nil
}

// nextElement returns the next start element at this level.
func nextElement(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.EndElement:
			return xml.StartElement{}, fmt.Errorf("value missing before </%s>", t.Name.Local)
		}
	}
}

func nextString(dec *xml.Decoder) (string, error) {
	start, err := nextElement(dec)
	if err != nil {
		return "", err
	}
	var v string
	if start.Name.Local != "string" {
		return "", fmt.Errorf("<%s>, want <string>", start.Name.Local)
	}
	err = dec.DecodeElement(&v, &start)
	return v, err
}

func stringArray(dec *xml.Decoder) ([]string, error) {
	start, err := nextElement(dec)
	if err != nil {
		return nil, err
	}
	var v struct {
		Strings []string `xml:"string"`
	}
	if start.Name.Local != "array" {
		return nil, fmt.Errorf("<%s>, want <array>", start.Name.Local)
	}
	err = dec.DecodeElement(&v, &start)
	return v.Strings, err
}

func stringDict(dec *xml.Decoder) (map[string]string, error) {
	start, err := nextElement(dec)
	if err != nil {
		return nil, err
	}
	if start.Name.Local != "dict" {
		return nil, fmt.Errorf("<%s>, want <dict>", start.Name.Local)
	}
	var v struct {
		Items []struct {
			XMLName xml.Name
			Text    string `xml:",chardata"`
		} `xml:",any"`
	}
	if err := dec.DecodeElement(&v, &start); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := 0; i+1 < len(v.Items); i += 2 {
		if v.Items[i].XMLName.Local == "key" && v.Items[i+1].XMLName.Local == "string" {
			out[v.Items[i].Text] = v.Items[i+1].Text
		}
	}
	return out, nil
}
