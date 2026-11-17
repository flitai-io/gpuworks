package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// validateChatInput checks the model's current editable declarations, like the
// Playground catalog. Missing declarations remain compatible with older models.
func (p *Proxy) validateChatInput(r *http.Request, deploymentID uint) error {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || !isJSONRequest(r) {
		return nil
	}
	modelID, err := p.deploymentModel(deploymentID)
	if err != nil {
		return err
	}
	supported, err := p.modalities.forModel(modelID)
	if err != nil || len(supported) == 0 {
		return err
	}
	if r.GetBody == nil {
		return nil
	}
	body, err := r.GetBody()
	if err != nil {
		return err
	}
	defer body.Close()
	return scanChatInput(bufio.NewReader(body), nil, func(modality string) error {
		if slices.Contains(supported, modality) {
			return nil
		}
		return refuse(ErrorModalityUnsupported, map[string]any{"modality": modality, "supported": strings.Join(supported, ", ")})
	})
}

// deploymentModel is the model a deployment serves: from the route table
// (a deployment's model never changes), else read.
func (p *Proxy) deploymentModel(deploymentID uint) (uint, error) {
	if rd := p.routes.deployment(deploymentID); rd != nil && rd.dep != nil {
		return rd.dep.ModelID, nil
	}
	dep, err := p.store.GetDeployment(deploymentID)
	if err != nil {
		return 0, err
	}
	return dep.ModelID, nil
}

// Walk the JSON structure without retaining media strings. Only the bounded
// keys and content-part type are decoded; quoted text/tool arguments are not parts.
func scanChatInput(reader *bufio.Reader, path []string, check func(string) error) error {
	first, err := nextJSONByte(reader)
	if err != nil {
		return err
	}
	wanted := []string{"messages", "*", "content", "*", "type"}
	for index, key := range path {
		if index >= len(wanted) || key != wanted[index] {
			return skipChatJSONValue(reader, first)
		}
	}
	switch first {
	case '{':
		for {
			token, err := nextJSONByte(reader)
			if err != nil {
				return err
			}
			if token == '}' {
				return nil
			}
			if token != '"' {
				return fmt.Errorf("invalid chat JSON object")
			}
			key, err := smallJSONString(reader)
			if err != nil {
				return err
			}
			token, err = nextJSONByte(reader)
			if err != nil {
				return err
			}
			if token != ':' {
				return fmt.Errorf("invalid chat JSON member")
			}
			if err := scanChatInput(reader, append(path, key), check); err != nil {
				return err
			}
			token, err = nextJSONByte(reader)
			if err != nil {
				return err
			}
			if token == '}' {
				return nil
			}
			if token != ',' {
				return fmt.Errorf("invalid chat JSON separator")
			}
		}
	case '[':
		token, err := nextJSONByte(reader)
		if err != nil {
			return err
		}
		if token == ']' {
			return nil
		}
		_ = reader.UnreadByte()
		for {
			if err := scanChatInput(reader, append(path, "*"), check); err != nil {
				return err
			}
			token, err = nextJSONByte(reader)
			if err != nil {
				return err
			}
			if token == ']' {
				return nil
			}
			if token != ',' {
				return fmt.Errorf("invalid chat JSON array")
			}
		}
	case '"':
		if strings.Join(path, "/") != "messages/*/content/*/type" {
			_, _, err := readJSONString(reader, false)
			return err
		}
		kind, err := smallJSONString(reader)
		if err != nil {
			return err
		}
		modality := ""
		switch kind {
		case "image_url":
			modality = "image"
		case "input_audio", "audio_url":
			modality = "audio"
		case "video_url":
			modality = "video"
		}
		if modality != "" {
			return check(modality)
		}
		return nil
	default:
		// Scalars have no media facts. Their exact syntax remains the engine's
		// validation responsibility, as before this content-type check.
		for {
			token, err := reader.ReadByte()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if token == ',' || token == '}' || token == ']' {
				return reader.UnreadByte()
			}
		}
	}
}

func nextJSONByte(reader *bufio.Reader) (byte, error) {
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b, nil
		}
	}
}

func smallJSONString(reader *bufio.Reader) (string, error) {
	raw := []byte{'"'}
	escaped := false
	oversized := false
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return "", err
		}
		if len(raw) < 1024 {
			raw = append(raw, b)
		} else {
			oversized = true
		}
		if b == '"' && !escaped {
			if oversized {
				return "", nil
			}
			var value string
			err := json.Unmarshal(raw, &value)
			return value, err
		}
		if b == '\\' && !escaped {
			escaped = true
		} else {
			escaped = false
		}
	}
}

// writeChatInputError answers a chat body that names a modality the model
// does not take with that refusal, and one the scan could not read with
// CHAT_INPUT_UNREADABLE.
func writeChatInputError(w http.ResponseWriter, r *http.Request, err error) {
	var rejection *refusal
	if !errors.As(err, &rejection) {
		rejection = refuse(ErrorChatInputUnreadable, nil)
	}
	writeRefusal(w, r, rejection)
}

// Unknown fields, tool schemas and media values cannot contain chat content
// parts. Skip their bytes without decoding strings or imposing nesting limits.
func skipChatJSONValue(reader *bufio.Reader, first byte) error {
	if first == '"' {
		_, _, err := readJSONString(reader, false)
		return err
	}
	if first != '{' && first != '[' {
		for {
			b, err := reader.ReadByte()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if b == ',' || b == '}' || b == ']' {
				return reader.UnreadByte()
			}
		}
	}
	depth := 1
	for depth > 0 {
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		switch b {
		case '"':
			if _, _, err := readJSONString(reader, false); err != nil {
				return err
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return nil
}

func (p *Proxy) rejectChatInput(w http.ResponseWriter, r *http.Request, tgt target, deploymentID uint) bool {
	if err := p.validateChatInput(r, deploymentID); err != nil {
		p.releaseTarget(tgt)
		p.acctFor(r.Context()).RecordImmediate(deploymentID, http.StatusBadRequest, false)
		writeChatInputError(w, r, err)
		return true
	}
	return false
}
