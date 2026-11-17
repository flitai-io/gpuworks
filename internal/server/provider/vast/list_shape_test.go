package vast

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Sync removes rows for instances the listing omits, so an unrecognised body
// must be an error rather than "no instances".
func TestListInstancesRejectsUnknownShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantIDs []string
		wantErr bool
	}{
		{name: "empty envelope", body: `{"success":true,"instances":[],"instances_found":0,"total_instances":0,"next_token":null}`, wantIDs: []string{}},
		{name: "bare array without pagination metadata", body: `[{"id":7}]`, wantErr: true},
		{name: "error object with 200", body: `{"success":false,"msg":"rate limited"}`, wantErr: true},
		{name: "null instances", body: `{"instances":null}`, wantErr: true},
		{name: "null body", body: `null`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ListInstances(%s) = %+v, want error", tc.body, insts)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListInstances(%s): %v", tc.body, err)
			}
			ids := make([]string, 0, len(insts))
			for _, inst := range insts {
				ids = append(ids, inst.ExternalID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}
