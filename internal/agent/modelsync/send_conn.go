package modelsync

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
)

// SendOnConnWithMagic sends an archive through a caller-provided raw transport.
// It takes ownership of raw and closes it on every return path. Authentication,
// framing and the receiver's byte/hash confirmation match SendWithMagic.
func SendOnConnWithMagic(ctx context.Context, raw net.Conn, token, fingerprint, magic string, writeArchive func(io.Writer) error, observe ...func(string)) (Result, error) {
	if raw == nil {
		return Result{}, errors.New("model sync connection is required")
	}
	defer raw.Close()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(fingerprint) == "" {
		return Result{}, errors.New("model sync credentials are incomplete")
	}
	if writeArchive == nil {
		return Result{}, errors.New("model sync archive callback is required")
	}
	if strings.TrimSpace(magic) == "" {
		magic = protocolMagic
	}
	conn, err := pinnedConn(ctx, raw, fingerprint)
	if err != nil {
		return Result{}, err
	}
	return sendArchive(ctx, conn, token, magic, writeArchive, true, observe...)
}

// sendArchive accepts only an authenticated connection. Both physical routes
// use this exact wire exchange; switching transports never restarts a callback.
func sendArchive(ctx context.Context, conn *tls.Conn, token, magic string, writeArchive func(io.Writer) error, halfClose bool, observe ...func(string)) (result Result, resultErr error) {
	notify := func(phase string) {
		for _, fn := range observe {
			if fn != nil {
				fn(phase)
			}
		}
	}
	notify("connected")
	defer func() {
		if resultErr != nil && ctx.Err() != nil {
			resultErr = ctx.Err()
		}
	}()
	connectionDone := make(chan struct{})
	defer close(connectionDone)
	go func() {
		select {
		case <-ctx.Done():
			// Abort the underlying stream rather than waiting for a TLS close
			// notification on a relay whose peer may no longer be reading.
			_ = conn.NetConn().Close()
		case <-connectionDone:
		}
	}()
	if err := writeJSONLine(conn, header{Magic: magic, Token: token}); err != nil {
		return Result{}, fmt.Errorf("write model sync header: %w", err)
	}
	cw := newChunkWriter(conn)
	if err := writeArchive(cw); err != nil {
		slog.Warn("model sync archive stream failed", "err", err)
		return Result{}, fmt.Errorf("stream model archive: %w", err)
	}
	if err := cw.Finish(); err != nil {
		return Result{}, fmt.Errorf("finish model archive: %w", err)
	}
	notify("confirming")
	// A fast receiver can send a valid ACK and close before the transport's
	// write-half shutdown. Retain shutdown errors for a failed read, but let
	// authenticated byte/hash confirmation decide whether the archive arrived.
	var halfCloseErr error
	if halfClose {
		if raw, ok := conn.NetConn().(interface{ CloseWrite() error }); ok {
			if err := conn.CloseWrite(); err != nil {
				halfCloseErr = fmt.Errorf("finish peer TLS write: %w", err)
			}
			if err := raw.CloseWrite(); err != nil {
				halfCloseErr = errors.Join(halfCloseErr, fmt.Errorf("finish peer transport write: %w", err))
			}
		}
	}
	var ack acknowledgement
	if err := json.NewDecoder(conn).Decode(&ack); err != nil {
		return Result{}, errors.Join(halfCloseErr, fmt.Errorf("read model sync acknowledgement: %w", err))
	}
	if !ack.OK {
		return Result{}, fmt.Errorf("target rejected model archive: %s", ack.Error)
	}
	digest := hex.EncodeToString(cw.hash.Sum(nil))
	if ack.Bytes != cw.bytes || !secureEqual(ack.SHA256, digest) {
		return Result{}, fmt.Errorf("model sync confirmation mismatch: sent %d/%s, target %d/%s", cw.bytes, digest, ack.Bytes, ack.SHA256)
	}
	slog.Info("model sync send completed", "bytes", ack.Bytes)
	return Result{Bytes: ack.Bytes, SHA256: ack.SHA256}, nil
}
