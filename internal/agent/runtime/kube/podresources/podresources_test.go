package podresources

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pb builds protobuf messages for the fake kubelet.
func varint(v uint64) []byte { return binary.AppendUvarint(nil, v) }

func field(num int, payload []byte) []byte {
	out := varint(uint64(num<<3 | 2))
	out = append(out, varint(uint64(len(payload)))...)
	return append(out, payload...)
}

func str(num int, s string) []byte { return field(num, []byte(s)) }

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func device(resource string, ids ...string) []byte {
	parts := [][]byte{str(1, resource)}
	for _, id := range ids {
		parts = append(parts, str(2, id))
	}
	// a topology field (3) the decoder must skip
	parts = append(parts, field(3, field(1, concat(varint(1<<3), varint(0)))))
	return concat(parts...)
}

func container(name string, devices ...[]byte) []byte {
	parts := [][]byte{str(1, name)}
	for _, d := range devices {
		parts = append(parts, field(2, d))
	}
	// cpu_ids (3) as packed varints and a varint field the decoder skips
	parts = append(parts, field(3, concat(varint(2), varint(3))), concat(varint(9<<3), varint(7)))
	return concat(parts...)
}

func pod(ns, name string, containers ...[]byte) []byte {
	parts := [][]byte{str(1, name), str(2, ns)}
	for _, c := range containers {
		parts = append(parts, field(3, c))
	}
	return field(1, concat(parts...))
}

func frame(msg []byte) []byte {
	out := []byte{0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(msg)))
	return append(out, msg...)
}

// fakeKubelet serves the PodResources List call over h2c on a Unix socket.
func fakeKubelet(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "podres")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "kubelet.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

func TestListDecodesAllocations(t *testing.T) {
	body := concat(
		pod("gpuworks-system", "gpuworks-hold-abc", container("hold", device("nvidia.com/gpu", "GPU-1111")), container("side")),
		pod("ml", "train-0", container("trainer", device("nvidia.com/gpu", "GPU-2222", "GPU-3333"), device("rdma/hca", "mlx5_0"))),
	)
	sock := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.URL.Path != listPath || r.Header.Get("Content-Type") != "application/grpc" {
			t.Errorf("request %s %s proto %d ct %q", r.Method, r.URL.Path, r.ProtoMajor, r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		_, _ = w.Write(frame(body))
		w.Header().Set("Grpc-Status", "0")
	})
	got, err := New(sock).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Allocation{
		{"gpuworks-system", "gpuworks-hold-abc", "hold", "nvidia.com/gpu", "GPU-1111"},
		{"ml", "train-0", "trainer", "nvidia.com/gpu", "GPU-2222"},
		{"ml", "train-0", "trainer", "nvidia.com/gpu", "GPU-3333"},
		{"ml", "train-0", "trainer", "rdma/hca", "mlx5_0"},
	}
	if len(got) != len(want) {
		t.Fatalf("List = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListReportsGRPCErrors(t *testing.T) {
	sock := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Message", "permission denied")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := New(sock).List(context.Background()); err == nil || !strings.Contains(err.Error(), "grpc-status 7") {
		t.Fatalf("err = %v", err)
	}
}

func TestListRejectsMalformedAnswers(t *testing.T) {
	for name, body := range map[string][]byte{
		"short":      {0, 0},
		"compressed": {1, 0, 0, 0, 0},
		"length":     {0, 0, 0, 0, 9, 1},
		"protobuf":   frame([]byte{0x0a, 0x7f}),
	} {
		sock := fakeKubelet(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/grpc")
			_, _ = w.Write(body)
		})
		if _, err := New(sock).List(context.Background()); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing.sock")).List(context.Background()); err == nil {
		t.Fatal("missing socket accepted")
	}
}
