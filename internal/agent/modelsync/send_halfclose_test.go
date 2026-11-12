package modelsync

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The receiver deliberately waits for both TLS EOF and transport EOF before
// acknowledging. A full Close would lose the acknowledgement, while omitting
// CloseWrite would leave the server waiting forever to start confirmation.
func TestRelaySendHalfClosesEncryptedOutputAndStillVerifiesAcknowledgement(t *testing.T) {
	cert, fp, err := ephemeralCertificate()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload := "confirmed archive"
	digest := sha256.Sum256([]byte(payload))
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
		reader := bufio.NewReader(conn)
		if _, err = readLine(reader, 64<<10); err != nil {
			done <- err
			return
		}
		n, err := io.Copy(io.Discard, &chunkReader{r: reader})
		if err != nil {
			done <- err
			return
		}
		if n != int64(len(payload)) {
			done <- fmt.Errorf("archive length=%d", n)
			return
		}
		if _, err = reader.ReadByte(); err != io.EOF {
			done <- fmt.Errorf("TLS output not closed: %v", err)
			return
		}
		var b [1]byte
		if _, err = raw.Read(b[:]); err != io.EOF {
			done <- fmt.Errorf("raw output not closed: %v", err)
			return
		}
		done <- writeJSONLine(conn, acknowledgement{OK: true, Bytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])})
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var phases []string
	result, err := SendOnConnWithMagic(ctx, raw, "token", fp, "", func(w io.Writer) error { _, err := io.WriteString(w, payload); return err }, func(phase string) { phases = append(phases, phase) })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if result.Bytes != int64(len(payload)) || result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("unverified result: %+v", result)
	}
	if !reflect.DeepEqual(phases, []string{"connected", "confirming"}) {
		t.Fatalf("phases=%v", phases)
	}
}

// The peer may already have closed its socket after writing its ACK. A local
// CloseWrite error is not evidence that the authenticated archive was lost.
type halfCloseFailureConn struct{ net.Conn }

func (c halfCloseFailureConn) CloseWrite() error { return fmt.Errorf("injected peer already closed") }

func TestRelayValidAcknowledgementWinsLateHalfCloseFailure(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint("reject=", reject), func(t *testing.T) {
			receiver, err := StartReceiver(context.Background(), ReceiveOptions{BindHost: "127.0.0.1", Token: "once", Receive: func(_ context.Context, r io.Reader) error {
				_, err := io.Copy(io.Discard, r)
				if reject {
					return fmt.Errorf("injected receiver validation failure")
				}
				return err
			}})
			if err != nil {
				t.Fatal(err)
			}
			defer receiver.Stop()
			raw, err := net.Dial("tcp", receiver.Endpoints[0])
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, err := SendOnConnWithMagic(ctx, halfCloseFailureConn{raw}, "once", receiver.Fingerprint, "", func(w io.Writer) error { _, err := io.WriteString(w, "verified"); return err })
			if reject {
				if err == nil || !strings.Contains(err.Error(), "injected receiver validation failure") {
					t.Fatalf("half-close fallback hid target rejection: %v", err)
				}
				return
			}
			if err != nil || result.Bytes != 8 {
				t.Fatalf("valid ACK lost: %+v %v", result, err)
			}
		})
	}
}
