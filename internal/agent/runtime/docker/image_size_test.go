package docker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestInspectImageRuntimeSize(t *testing.T) {
	for _, tc := range []struct {
		name, size string
		want       any
	}{
		{"measured", `,"Size":8192`, float64(8192)},
		{"zero", `,"Size":0`, float64(0)},
		{"missing", "", nil}, {"negative", `,"Size":-1`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeDocker(t, `test "$1 $2 $3 $4 $5" = 'image inspect --format {{json .}} image:v1' || exit 9
printf '%s\n' '{"Id":"sha256:example","Config":{}`+tc.size+`}'`)
			got, err := New().InspectImage(context.Background(), "image:v1")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			var fields map[string]any
			_ = json.Unmarshal(raw, &fields)
			if fields["RuntimeSizeBytes"] != tc.want {
				t.Fatalf("runtime size=%v want=%v, inspection=%s", fields["RuntimeSizeBytes"], tc.want, raw)
			}
		})
	}
}

func TestImagePresenceUsesOneInspectionAndDistinguishesFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		present, wantErr bool
		id               string
		size             *int64
	}{
		{name: "present", body: `printf '%s\n' '{"Id":"sha256:one","Size":8192,"Config":{}}'`, present: true, id: "sha256:one", size: func() *int64 { v := int64(8192); return &v }()},
		{name: "missing", body: `echo 'Error response from daemon: No such image: image:v1' >&2; exit 1`},
		{name: "daemon error", body: `echo 'Cannot connect to the Docker daemon' >&2; exit 1`, wantErr: true},
		{name: "malformed", body: `echo '{'`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := t.TempDir() + "/calls"
			t.Setenv("INSPECTION_CALLS", calls)
			installFakeDocker(t, `echo call >> "$INSPECTION_CALLS"
test "$1 $2 $3 $4 $5" = 'image inspect --format {{json .}} image:v1' || exit 9
`+tc.body)
			present, id, size, err := New().ImagePresence(context.Background(), "image:v1")
			if present != tc.present || id != tc.id || (err != nil) != tc.wantErr || (size == nil) != (tc.size == nil) || size != nil && *size != *tc.size {
				t.Fatalf("present=%v id=%q size=%v err=%v", present, id, size, err)
			}
			raw, err := os.ReadFile(calls)
			if err != nil || string(raw) != "call\n" {
				t.Fatalf("calls=%q err=%v", raw, err)
			}
		})
	}
}

// The inventory asks about every absent catalog image every minute: Docker's
// "No such image" is that question's answer and is logged at Debug, while a
// command that really failed stays a warning.
func TestImageInspectLogsAbsenceBelowWarn(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		warn       bool
	}{
		{"missing", `echo 'Error response from daemon: No such image: image:v1' >&2; exit 1`, false},
		{"daemon error", `echo 'Cannot connect to the Docker daemon' >&2; exit 1`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged strings.Builder
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })
			installFakeDocker(t, tc.body)
			_, _, _, _ = New().ImagePresence(context.Background(), "image:v1")
			line := logged.String()
			if !strings.Contains(line, `msg="runtime command failed"`) || strings.Contains(line, "level=WARN") != tc.warn {
				t.Fatalf("logged:\n%s", line)
			}
		})
	}
}
