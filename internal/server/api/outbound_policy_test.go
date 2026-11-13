package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

type editableOutboundSettings struct {
	StaticServerSettings
	value  dto.ServerOutboundPolicy
	writes int
	err    error
}

func (s *editableOutboundSettings) OutboundPolicy() (dto.ServerOutboundPolicy, error) {
	return s.value, s.err
}
func (s *editableOutboundSettings) UpdateOutboundPolicy(u dto.ServerOutboundPolicyUpdate) (dto.ServerOutboundPolicy, error) {
	s.writes++
	if s.err != nil {
		return dto.ServerOutboundPolicy{}, s.err
	}
	s.value.RestrictPrivateHosts = *u.RestrictPrivateHosts
	s.value.AllowPrivateHosts = u.AllowPrivateHosts
	return s.value, nil
}
func TestOutboundPolicyRoutesAndPayload(t *testing.T) {
	signer, err := auth.NewTokenSigner("outbound-settings-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	admin, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	settings := &editableOutboundSettings{value: dto.ServerOutboundPolicy{AllowPrivateHosts: []string{}, Editable: true, Revision: "initial"}}
	router := Router(Deps{Store: st, AuthSigner: signer, Server: settings})
	const path = "/api/v1/server/outbound-policy"
	for _, method := range []string{"GET", "PUT"} {
		for _, token := range []string{"", viewer} {
			want := 403
			if token == "" {
				want = 401
			}
			rec := serverSettingsCall(router, method, path, token, `{}`)
			if rec.Code != want {
				t.Fatalf("%s auth status=%d %s", method, rec.Code, rec.Body)
			}
		}
	}
	if settings.writes != 0 {
		t.Fatal("unauthorized mutation")
	}
	rec := serverSettingsCall(router, "GET", path, admin, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"restrict_private_hosts":false,"allow_private_hosts":[],"editable":true,"revision":"initial"}` {
		t.Fatalf("GET %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{}`, `null`, `{"restrict_private_hosts":false,"allow_private_hosts":null,"revision":"r"}`, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":""}`, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"r","unexpected":1}`, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"r"} {}`, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"r","padding":"` + strings.Repeat("x", 65536) + `"}`} {
		rec = serverSettingsCall(router, "PUT", path, admin, body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "SERVER_OUTBOUND_POLICY_INVALID") {
			t.Fatalf("invalid body status=%d %s", rec.Code, rec.Body)
		}
	}
	if settings.writes != 0 {
		t.Fatal("invalid request mutated policy")
	}
	rec = serverSettingsCall(router, "PUT", path, admin, `{"restrict_private_hosts":true,"allow_private_hosts":["127.0.0.1"],"revision":"initial"}`)
	if rec.Code != 200 || settings.writes != 1 || !settings.value.RestrictPrivateHosts {
		t.Fatalf("PUT %d %s", rec.Code, rec.Body)
	}
	if rec = serverSettingsCall(router, http.MethodDelete, path, admin, ""); rec.Code != 405 {
		t.Fatalf("method %d", rec.Code)
	}
	settings.err = &ServerSettingsError{Code: ErrorServerOutboundPolicyConflict, Fallback: "reload policy"}
	if rec = serverSettingsCall(router, "PUT", path, admin, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"initial"}`); rec.Code != 409 {
		t.Fatalf("conflict %d %s", rec.Code, rec.Body)
	}
	settings.err = errors.New("secret YAML value")
	rec = serverSettingsCall(router, "GET", path, admin, "")
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "secret YAML value") || !strings.Contains(rec.Body.String(), ErrorServerOutboundPolicyUnavailable) {
		t.Fatalf("unsafe failure %d %s", rec.Code, rec.Body)
	}
	readonly := Router(Deps{Store: st, AuthSigner: signer})
	if rec = serverSettingsCall(readonly, "PUT", path, admin, `{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"r"}`); rec.Code != 409 {
		t.Fatalf("readonly %d %s", rec.Code, rec.Body)
	}
}
