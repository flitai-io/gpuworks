// Package extmodule holds the contract tests of the server's extension points
// (docs/dev/extensions.md): a fake module, a fake feature source, a fake node
// insert guard, a fake scheduling holder and a fake card holder, each driven
// through a real
// server the way a separately built one composes them.
package extmodule

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const featureKey featuregate.Key = "fake.reports"

// fakeSource answers every feature from one switchable access, the way a
// real source answers from an atomic snapshot.
type fakeSource struct{ access atomic.Uint32 }

func newFakeSource(a featuregate.Access) *fakeSource {
	s := &fakeSource{}
	s.set(a)
	return s
}

func (s *fakeSource) set(a featuregate.Access) { s.access.Store(uint32(a)) }

func (s *fakeSource) Availability(featuregate.Key) featuregate.Availability {
	return featuregate.Availability{Access: featuregate.Access(s.access.Load()), Link: "https://example.com/fake"}
}

// fakeHolder holds every node with cards while on, and counts how often it
// was asked.
type fakeHolder struct {
	on    atomic.Bool
	asked atomic.Int64
}

func (h *fakeHolder) Source() store.SchedulingSource { return "fakehold" }

func (h *fakeHolder) Hold(f store.NodeHoldFacts) store.NodeHold {
	h.asked.Add(1)
	if !h.on.Load() || f.OwnedCards == 0 {
		return store.NodeHold{}
	}
	return store.NodeHold{Code: "FAKE_HOLD", Fallback: "the fake module holds this node", Params: map[string]any{"cards": f.OwnedCards}}
}

// fakeCardHolder holds the one card whose identity it is given, from an
// atomic snapshot the way a real card holder answers, and counts how often
// it was asked.
type fakeCardHolder struct {
	ident atomic.Pointer[string]
	asked atomic.Int64
}

func (h *fakeCardHolder) Source() store.SchedulingSource { return "fakecard" }

func (h *fakeCardHolder) hold(ident string) { h.ident.Store(&ident) }

func (h *fakeCardHolder) release() { h.ident.Store(nil) }

// cardHold is the hold it answers with, built once: refusing allocates
// nothing either.
var cardHold = store.CardHold{Code: "FAKE_CARD_HOLD", Fallback: "the fake module holds this card", Params: map[string]any{"reason": "fake"}}

func (h *fakeCardHolder) HoldCard(f store.CardHoldFacts) store.CardHold {
	h.asked.Add(1)
	if p := h.ident.Load(); p != nil && *p == f.Ident {
		return cardHold
	}
	return store.CardHold{}
}

// fakeRoutes are the fake module's API: a community GET, and a GET and a
// POST behind the feature.
type fakeRoutes struct{}

func (fakeRoutes) Name() string { return "fake" }

func (fakeRoutes) Routes(m api.Mounter) {
	ok := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	m.Handle(api.RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/ping", Action: "fake.ping", Community: true}, ok(`{"pong":true}`))
	m.Handle(api.RouteSpec{Method: http.MethodGet, Pattern: "/x/fake/reports", Action: "fake.list", Feature: featureKey}, ok(`{"reports":[]}`))
	m.Handle(api.RouteSpec{Method: http.MethodPost, Pattern: "/x/fake/reports", Access: api.AccessAdmin, Action: "fake.create", Feature: featureKey}, ok(`{"created":true}`))
}

func (fakeRoutes) Nav() []dto.NavContribution {
	return []dto.NavContribution{{ID: "fake-reports", Group: "admin", LabelKey: "fake:nav.reports", Route: "/fake", Feature: string(featureKey)}}
}

type fakeBanners struct{}

func (fakeBanners) Banners(context.Context, auth.Claims) []dto.ConsoleBanner {
	return []dto.ConsoleBanner{{ID: "fake-notice", Severity: "warning", Message: dto.ProductMessage{Code: "FAKE_NOTICE", Message: "fake notice"}}}
}

// fakeConfig is the module's server.yaml section.
type fakeConfig struct {
	Greeting string `yaml:"greeting"`
}

// fakeModule uses every Host method from Init and records what it saw.
type fakeModule struct {
	name    string
	source  *fakeSource
	holder  *fakeHolder
	cards   *fakeCardHolder
	guard   store.NodeEnrollmentGuard
	limiter api.PlatformLimiter
	initErr error

	mu        sync.Mutex
	calls     []string
	host      *app.Host
	cfg       fakeConfig
	installID string
	dataDir   string
	keys      map[string][]byte
	tables    *store.ExtensionTables
	runs      atomic.Int32
	running   atomic.Int32
	order     *[]string // shared across modules: Init order
}

func (m *fakeModule) Name() string { return m.name }

func (m *fakeModule) Migrations() store.MigrationSet {
	return store.MigrationSet{Namespace: m.name, TablePrefix: "ee_" + m.name + "_", Migrations: []store.Migration{{
		Version: 1, Name: "records",
		Apply: func(tx *gorm.DB) error {
			return tx.Exec("CREATE TABLE ee_" + m.name + "_records (id INTEGER PRIMARY KEY, note TEXT NOT NULL)").Error
		},
	}}}
}

func (m *fakeModule) record(call string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
}

func (m *fakeModule) Init(_ context.Context, h *app.Host) error {
	if m.order != nil {
		*m.order = append(*m.order, m.name)
	}
	if m.initErr != nil {
		return m.initErr
	}
	m.host = h
	h.Log().Info("fake module starting")
	m.record("log")
	if h.Bus() == nil {
		return errFake("no bus")
	}
	m.record("bus")
	m.dataDir = h.DataDir()
	m.installID = h.InstallID()
	if err := h.Section(&m.cfg); err != nil {
		return err
	}
	m.record("section")
	m.tables = h.Tables()
	if m.tables != nil {
		if err := m.tables.Write(func(tx *gorm.DB) error {
			return tx.Exec("INSERT INTO ee_"+m.name+"_records (note) VALUES (?)", "from init").Error
		}); err != nil {
			return err
		}
		m.record("tables")
	}
	m.keys = map[string][]byte{}
	for _, purpose := range []string{"signing", "sealing"} {
		k, err := h.DeriveKey(purpose)
		if err != nil {
			return err
		}
		m.keys[purpose] = k
	}
	m.record("derive")
	if m.name != "fake" {
		return nil // a second module only proves ordering and isolation
	}
	h.DeclareFeatures(featuregate.Feature{Key: featureKey})
	if m.source != nil {
		if err := h.ProvideFeatures(m.source); err != nil {
			return err
		}
	}
	h.MountAPI(fakeRoutes{})
	h.AddBannerSource(fakeBanners{})
	if m.guard != nil {
		h.GuardNodeInsert(m.guard)
	}
	if m.holder != nil {
		if err := h.HoldScheduling(m.holder); err != nil {
			return err
		}
	}
	if m.cards != nil {
		if err := h.HoldCardScheduling(m.cards); err != nil {
			return err
		}
	}
	if m.limiter != nil {
		if err := h.LimitPlatform(m.limiter); err != nil {
			return err
		}
	}
	m.record("registered")
	return nil
}

func (m *fakeModule) Run(ctx context.Context) error {
	m.runs.Add(1)
	m.running.Add(1)
	defer m.running.Add(-1)
	<-ctx.Done()
	return ctx.Err()
}

type errFake string

func (e errFake) Error() string { return string(e) }

// productError decodes an API error body.
func productError(body []byte) (code string, params map[string]any) {
	var out struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Code, out.Params
}

// refuseFromSecond refuses every node after the first.
func refuseFromSecond(c store.FleetCapacity) *messages.ProductError {
	if c.Nodes < 1 {
		return nil
	}
	return messages.NewProductError("FAKE_NODE_CAP", "the fake module allows one node", map[string]any{"nodes": c.Nodes})
}
