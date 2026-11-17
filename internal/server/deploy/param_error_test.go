package deploy

import (
	"errors"
	"fmt"
	"testing"
)

func TestParamErrorNamesTheFieldThroughWrapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		key  string
		text string
	}{
		{"args text", func() error { _, err := ParseArgsText(`--x "open`); return err }(), ParamFieldExtraArgs, "argument text has an unterminated quote"},
		{"args json", func() error { _, err := ParseArgsText(`["a",`); return err }(), ParamFieldExtraArgs, ""},
		{"unset", func() error {
			_, err := Materialize(MaterializeInput{Engine: "vllm", Unset: []string{"no_such_key"}})
			return err
		}(), ParamFieldQuickParamsUnset, `invalid quick_params_unset: unknown key "no_such_key"`},
		{"cross key", ValidateLaunchConfigForEngine("vllm", LaunchConfig{MaxNumBatchedTokens: 8, MaxNumSeqs: 64}), "max_num_batched_tokens", "max_num_batched_tokens must be at least max_num_seqs"},
		{"dependency", ValidateLaunchConfigForEngine("vllm", LaunchConfig{KVDiskOffload: true, SharedKVDir: "/kv"}), "kv_disk_offload", "kv_disk_offload requires kv_offloading_size to be enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paramErr *ParamError
			if !errors.As(fmt.Errorf("invalid quick_params: %w", tc.err), &paramErr) || paramErr.Key != tc.key {
				t.Fatalf("err = %v (%#v), want ParamError key %q", tc.err, paramErr, tc.key)
			}
			if tc.text != "" && tc.err.Error() != tc.text {
				t.Fatalf("text = %q, want %q", tc.err, tc.text)
			}
		})
	}
}
