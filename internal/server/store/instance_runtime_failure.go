package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ErrFailedNeedsReason refuses a failed transition without a structured
// reason: SetInstanceFailed is the server's only way to fail an instance.
var ErrFailedNeedsReason = errors.New("an instance is failed only with a runtime failure (SetInstanceFailed)")

// eventFailureParam is the instance event param that carries the structured
// reason of a failed transition next to its detail (the English fallback).
const eventFailureParam = "failure"

// Failure is the structured reason the instance failed, read whole from its
// row with its codes in the one spelling; nil when the row has none (an
// older agent's report, a row written before reasons existed, or a state
// that is not a failure). An older agent's port refusal is the one failure
// it already named by a code, as the prefix of its message; that row reads
// as the coded reason (legacyPortRefusal), so the server decides by code.
func (in *Instance) Failure() *proto.RuntimeFailure {
	if failure := decodeRuntimeFailure(in.RuntimeFailure); failure != nil {
		return failure
	}
	if in.State == proto.InstFailed {
		return legacyPortRefusal(in.Message)
	}
	return nil
}

// legacyPortRefusalPrefix is how an agent that predates RuntimeFailure
// reported a port refusal: "ENGINE_PORT_REFUSED: port 30000 is already in
// use on this node; nothing was started". Reading it is a permanent
// compatibility boundary with older agents (ADR 0067), and this is the only
// place the message is parsed.
const legacyPortRefusalPrefix = proto.ErrorEnginePortRefused + ": port "

func legacyPortRefusal(message string) *proto.RuntimeFailure {
	rest, ok := strings.CutPrefix(message, legacyPortRefusalPrefix)
	if !ok {
		return nil
	}
	failure := &proto.RuntimeFailure{Code: proto.ErrorEnginePortRefused, Phase: proto.PhaseContainerPrepare}
	if digits, _, _ := strings.Cut(rest, " "); digits != "" {
		if port, err := strconv.Atoi(digits); err == nil && port > 0 {
			failure.Port = port
		}
	}
	return failure
}

func decodeRuntimeFailure(raw string) *proto.RuntimeFailure {
	if raw == "" {
		return nil
	}
	var failure proto.RuntimeFailure
	if json.Unmarshal([]byte(raw), &failure) != nil || failure.Code == "" {
		return nil
	}
	failure = failure.Normalized()
	return &failure
}

// storedRuntimeFailure is the column value for a reported reason: bounded
// again, since an agent is not trusted to, and in the one spelling.
func storedRuntimeFailure(failure *proto.RuntimeFailure) string {
	if failure == nil || failure.Code == "" {
		return ""
	}
	return string(mustJSON(failure.Bounded().Normalized()))
}

// withEventFailure adds the reason of a failed transition to its event's
// params, so the timeline renders it from its code.
func withEventFailure(params map[string]any, failure *proto.RuntimeFailure) map[string]any {
	if failure != nil && failure.Code != "" {
		params[eventFailureParam] = failure.Bounded().Normalized()
	}
	return params
}

// EventFailure is the structured reason an instance event's params carry
// (written by a failed transition); nil when they carry none.
func EventFailure(params map[string]any) *proto.RuntimeFailure {
	value, ok := params[eventFailureParam]
	if !ok {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return decodeRuntimeFailure(string(raw))
}

// SetInstanceFailed is the server's one way to fail an instance: the reason
// is stored whole with the message (its English fallback) and goes on the
// timeline event with it.
func (s *Store) SetInstanceFailed(uuid string, failure proto.RuntimeFailure, message string) error {
	if failure.Code == "" {
		return ErrFailedNeedsReason
	}
	return s.setInstanceState(uuid, proto.InstFailed, message, &failure)
}

// SetInstanceState updates just the state (server-driven transitions). A
// failure goes through SetInstanceFailed, which requires its reason.
func (s *Store) SetInstanceState(uuid, state, message string) error {
	if state == proto.InstFailed {
		return ErrFailedNeedsReason
	}
	return s.setInstanceState(uuid, state, message, nil)
}

func (s *Store) setInstanceState(uuid, state, message string, failure *proto.RuntimeFailure) error {
	inst, err := s.InstanceByUUID(uuid)
	if err != nil {
		return err
	}
	if err := s.db.Model(&Instance{}).Where("id = ?", inst.ID).
		Updates(map[string]any{"state": state, "message": message, "runtime_failure": storedRuntimeFailure(failure)}).Error; err != nil {
		return err
	}
	s.Bus.Advance("instance") // announced after the event row; see Bus.Advance
	s.AddInstanceEventCode(inst, "state", instanceEventStateChanged, withEventFailure(map[string]any{"state": state, "detail": message}, failure))
	s.Bus.PublishChange("instance", uuid, state)
	return nil
}
