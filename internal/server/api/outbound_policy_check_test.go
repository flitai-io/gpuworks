package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

type outboundCheckSettings struct{ editableOutboundSettings }

func (s *outboundCheckSettings) UpdateOutboundPolicy(update dto.ServerOutboundPolicyUpdate) (dto.ServerOutboundPolicy, error) {
	if !s.value.Editable {
		return dto.ServerOutboundPolicy{}, &ServerSettingsError{Code: ErrorServerOutboundPolicyReadOnly}
	}
	if update.Revision != s.value.Revision {
		return dto.ServerOutboundPolicy{}, &ServerSettingsError{Code: ErrorServerOutboundPolicyConflict}
	}
	s.writes++
	s.value.RestrictPrivateHosts = *update.RestrictPrivateHosts
	s.value.AllowPrivateHosts = append([]string{}, update.AllowPrivateHosts...)
	s.value.Revision = fmt.Sprintf("updated-%d", s.writes)
	return s.value, nil
}

func (s *outboundCheckSettings) OutboundAllowPrivateHosts() []string {
	if !s.value.RestrictPrivateHosts {
		return []string{"0.0.0.0/0", "::/0"}
	}
	return append([]string(nil), s.value.AllowPrivateHosts...)
}

func TestOutboundPolicyCheckExplainsPrivateHostWithoutGrantingIt(t *testing.T) {
	st := managementSessionTestStore(t)
	signer, err := auth.NewTokenSigner("outbound-host-check", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	settings := &outboundCheckSettings{editableOutboundSettings{value: dto.ServerOutboundPolicy{RestrictPrivateHosts: true, AllowPrivateHosts: []string{}, Editable: true, Revision: "initial"}}}
	router := Router(Deps{Store: st, AuthSigner: signer, Server: settings})
	before := serverSettingsCall(router, http.MethodGet, "/api/v1/server/outbound-policy", admin, "")
	checked := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin, `{"hosts":["127.0.0.1"]}`)
	if checked.Code != http.StatusOK || strings.TrimSpace(checked.Body.String()) != `{"items":[{"host":"127.0.0.1","status":"needs_allow"}],"total":1}` {
		t.Fatalf("check status=%d body=%s", checked.Code, checked.Body)
	}
	after := serverSettingsCall(router, http.MethodGet, "/api/v1/server/outbound-policy", admin, "")
	if before.Code != http.StatusOK || after.Code != http.StatusOK || after.Body.String() != before.Body.String() || settings.writes != 0 {
		t.Fatalf("read-only check changed policy: before=%s after=%s writes=%d", before.Body, after.Body, settings.writes)
	}
}

func outboundCheckRouter(t *testing.T) (http.Handler, string, string, *outboundCheckSettings) {
	t.Helper()
	st := managementSessionTestStore(t)
	signer, err := auth.NewTokenSigner("outbound-host-boundaries", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: "member", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	settings := &outboundCheckSettings{editableOutboundSettings{value: dto.ServerOutboundPolicy{RestrictPrivateHosts: true, AllowPrivateHosts: []string{}, Editable: true, Revision: "initial"}}}
	return Router(Deps{Store: st, AuthSigner: signer, Server: settings}), admin, member, settings
}

func TestOutboundPolicyCheckRefusesURLsWithoutEchoingSecrets(t *testing.T) {
	router, admin, _, _ := outboundCheckRouter(t)
	checked := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin,
		`{"hosts":["http://operator:private-password@127.0.0.1/private-path?token=private-token"]}`)
	if checked.Code != http.StatusBadRequest || !strings.Contains(checked.Body.String(), `"code":"SERVER_OUTBOUND_POLICY_INVALID"`) {
		t.Fatalf("invalid host status=%d body=%s", checked.Code, checked.Body)
	}
	for _, secret := range []string{"operator", "private-password", "private-path", "private-token"} {
		if strings.Contains(checked.Body.String(), secret) {
			t.Fatalf("invalid host response exposed input: %s", checked.Body)
		}
	}
}

func TestOutboundPolicyCheckRequiresABoundedHostList(t *testing.T) {
	router, admin, _, _ := outboundCheckRouter(t)
	tooMany, err := json.Marshal(dto.ServerOutboundPolicyCheck{Hosts: strings.Split(strings.Repeat("8.8.8.8,", 32)+"8.8.8.8", ",")})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `null`, `{"hosts":null}`, `{"hosts":"127.0.0.1"}`, `{"hosts":[1]}`,
		`{"hosts":[],"unexpected":"private-token"}`, `{"hosts":[]} {}`, `{"hosts":["127.0.0.1"]`,
		string(tooMany), `{"hosts":[]}` + strings.Repeat(" ", 16<<10),
	} {
		checked := serverSettingsCall(router, http.MethodPost, "/api/v1/server/outbound-policy/check", admin, body)
		if checked.Code != http.StatusBadRequest || !strings.Contains(checked.Body.String(), `"code":"SERVER_OUTBOUND_POLICY_INVALID"`) {
			t.Errorf("invalid body %.100q: status=%d body=%s", body, checked.Code, checked.Body)
		}
		if strings.Contains(checked.Body.String(), "private-token") {
			t.Errorf("invalid body leaked its input: %s", checked.Body)
		}
	}
}
