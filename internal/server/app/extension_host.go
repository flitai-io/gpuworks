package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Host is what a module's Init reaches the server through. It hands out the
// module's own tables, never the Store or a raw database handle, and collects
// what the module registers; registrations are accepted only while Init runs.
type Host struct {
	name    string
	log     *slog.Logger
	bus     *bus.Bus
	dataDir string
	cfg     Config
	install string
	master  string
	section serverconfig.File
	tables  *store.ExtensionTables
	st      *store.Store
	reg     *extensionSet
	app     *App
}

// MemberAnswerer answers a question another member of the control-plane
// cluster asked this module (Host.AnswerMembers). It is called on the
// member route, for an admitted member's certificate only.
type MemberAnswerer func(ctx context.Context, question []byte) ([]byte, error)

// MemberQuestionLimit bounds a question to a member and its answer.
const MemberQuestionLimit = 64 << 10

var (
	// ErrNotClusterMember: this server is not a member of a control-plane
	// cluster, so it has nobody to ask.
	ErrNotClusterMember = errors.New("this server is not a member of a control-plane cluster")
	// ErrMemberRouteNotReady: this server is a cluster member whose route
	// to the other members is not up yet. A module's Init runs before the
	// route exists, so a question asked there always gets this; it says
	// nothing about the member asked.
	ErrMemberRouteNotReady = errors.New("this member's route to the other members is not up yet")
	// ErrMemberUnreachable: the member has no route, did not answer, or
	// nothing on it answers for the module.
	ErrMemberUnreachable = errors.New("the member gave no answer")
)

// AnswerMembers makes answer what this module tells the other members of
// the cluster that ask it (AskMember). A module registers one answerer; on
// a single server it is never called.
func (h *Host) AnswerMembers(answer MemberAnswerer) {
	h.mustBeOpen("AnswerMembers")
	if answer == nil {
		panic(fmt.Sprintf("extension %s: AnswerMembers(nil)", h.name))
	}
	if h.reg.answers == nil {
		h.reg.answers = map[string]MemberAnswerer{}
	}
	if _, dup := h.reg.answers[h.name]; dup {
		panic(fmt.Sprintf("extension %s: AnswerMembers called twice", h.name))
	}
	h.reg.answers[h.name] = answer
}

// AskMember asks this module on another member of the control-plane cluster
// a question and returns what its AnswerMembers function answered. member is
// a ClusterObservedMember's ID. The question travels over the members' own
// route: mutual TLS with the members' certificates, so the answer comes from
// the member asked and nobody else can ask. Question and answer are at most
// MemberQuestionLimit bytes. Unlike the registrations it is called while
// serving. A single server answers ErrNotClusterMember. A cluster member
// answers ErrMemberRouteNotReady until its route to the others is up, which
// is after every module's Init returned: the question was not asked. A
// member that is down, or runs a build whose module registered no answerer,
// answers ErrMemberUnreachable.
func (h *Host) AskMember(ctx context.Context, member uint64, question []byte) ([]byte, error) {
	if h.app == nil {
		return nil, ErrNotClusterMember
	}
	return h.app.askMember(ctx, member, h.name, question)
}

// ErrHostClosed is returned (or, for registrations without an error result,
// panicked with) when a module registers after its Init returned.
var ErrHostClosed = errors.New("extension host: registrations are accepted only while Init runs")

// Log is the server's logger, with module=<name> on every record.
func (h *Host) Log() *slog.Logger { return h.log }

// Bus is the server's event bus. A subscriber must not block the publisher.
func (h *Host) Bus() *bus.Bus { return h.bus }

// DataDir is the server's data directory. A module keeps its files under a
// directory named after itself.
func (h *Host) DataDir() string { return h.dataDir }

// InstallID names this installation; it is stable for the life of the data
// directory's master key and reveals nothing of it.
func (h *Host) InstallID() string { return h.install }

// Section decodes server.yaml's extensions.<name> into v, refusing keys v
// does not have. An absent section leaves v as it is.
func (h *Host) Section(v any) error { return h.section.Section(h.name, v) }

// Tables is the handle to the module's own tables (its Migrator's set); nil
// for a module without one. It is the only way a module reaches the
// database: core data goes through the API and the hooks below.
func (h *Host) Tables() *store.ExtensionTables { return h.tables }

var keyPurposePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// DeriveKey is a 32-byte secret for purpose, derived from the installation's
// master key with HKDF-SHA256 and bound to the module's name, so two modules,
// or two purposes of one module, never share a key and none reveals the
// master key. It is the same for the life of the master key. purpose follows
// [a-z][a-z0-9_.-]{0,63}; a module that needs a signing key seeds one with it
// (ed25519.NewKeyFromSeed).
func (h *Host) DeriveKey(purpose string) ([]byte, error) {
	if !keyPurposePattern.MatchString(purpose) {
		return nil, fmt.Errorf("extension %s: key purpose %q does not match [a-z][a-z0-9_.-]{0,63}", h.name, purpose)
	}
	return hkdf.Key(sha256.New, []byte(h.master), []byte("gpuworks extension key"), h.name+"\x00"+purpose, 32)
}

// MountAPI adds e's routes under /api/v1/x/<e.Name()>/ (api.Extension).
func (h *Host) MountAPI(e api.Extension) {
	h.mustBeOpen("MountAPI")
	if e == nil {
		panic(fmt.Sprintf("extension %s: MountAPI(nil)", h.name))
	}
	h.reg.apis = append(h.reg.apis, e)
}

// AddBannerSource adds notices above every console page (api.BannerSource).
func (h *Host) AddBannerSource(b api.BannerSource) {
	h.mustBeOpen("AddBannerSource")
	if b == nil {
		panic(fmt.Sprintf("extension %s: AddBannerSource(nil)", h.name))
	}
	h.reg.banners = append(h.reg.banners, b)
}

// DeclareFeatures declares feature keys the module's routes, menu entries
// and guards name. A key is declared once across all modules.
func (h *Host) DeclareFeatures(features ...featuregate.Feature) {
	h.mustBeOpen("DeclareFeatures")
	if h.reg.dynamic != nil {
		panic(fmt.Sprintf("extension %s: DeclareFeatures after the plugins module made the dynamic feature set", h.name))
	}
	if h.reg.featureBy == nil {
		h.reg.featureBy = map[featuregate.Key]string{}
	}
	for _, f := range features {
		if _, dup := h.reg.featureBy[f.Key]; !dup {
			h.reg.featureBy[f.Key] = h.name
		}
	}
	h.reg.features = append(h.reg.features, features...)
}

// ProvideFeatures makes src the answer for every feature the modules
// declare. At most one module provides it; without one, every declared
// feature is locked (hidden when concealed). The feature keys installed
// plugin packages declare are answered by the plugins module, beside it.
func (h *Host) ProvideFeatures(src featuregate.Source) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if src == nil {
		return fmt.Errorf("extension %s: ProvideFeatures(nil)", h.name)
	}
	if h.reg.sourceBy != "" {
		return fmt.Errorf("extension %s: features are already provided by %s", h.name, h.reg.sourceBy)
	}
	h.reg.source, h.reg.sourceBy = src, h.name
	return nil
}

// LimitPlatform makes src the answer for how far the whole server is
// available (api.PlatformLimiter): read-only refuses every change and keeps
// serving reads and the data plane; locked serves reads as well and refuses
// the data plane too. This module's own routes are served under either. At most one module limits the server;
// without one it is never limited. src runs on every request and must
// answer from a snapshot: one atomic load, no I/O, no lock, no allocation.
func (h *Host) LimitPlatform(src api.PlatformLimiter) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if src == nil {
		return fmt.Errorf("extension %s: LimitPlatform(nil)", h.name)
	}
	if h.reg.platformBy != "" {
		return fmt.Errorf("extension %s: the server is already limited by %s", h.name, h.reg.platformBy)
	}
	h.reg.platform, h.reg.platformBy = src, h.name
	return nil
}

// GuardNodeInsert adds g to every new node's insert (store.NodeEnrollmentGuard):
// guards run in registration order inside the insert's writer transaction,
// and the first refusal wins. g must be pure: it gets the fleet's counts and
// must not call the Store, the module's Tables or anything that waits on
// them.
func (h *Host) GuardNodeInsert(g store.NodeEnrollmentGuard) {
	h.mustBeOpen("GuardNodeInsert")
	if g == nil {
		panic(fmt.Sprintf("extension %s: GuardNodeInsert(nil)", h.name))
	}
	h.st.AddNodeEnrollmentGuard(g)
}

// HoldScheduling registers a holder that keeps nodes out of scheduling
// (store.SchedulingHolder): it is asked on every status report inside the
// writer's transaction, so it must be pure and O(1). Holders are keyed by
// their source; a second holder with a source already registered is refused.
// The server applies every holder's answers once Init has run, and again
// whenever a module calls ReapplySchedulingHolds.
func (h *Host) HoldScheduling(holder store.SchedulingHolder) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if err := h.st.AddSchedulingHolder(holder); err != nil {
		return fmt.Errorf("extension %s: %w", h.name, err)
	}
	h.reg.holders++
	return nil
}

// HoldCardScheduling registers a card holder that keeps single cards out of
// scheduling (store.CardSchedulingHolder): it is asked once per card on
// every status report that writes the node's cards, inside the writer's
// transaction, so it must be pure and O(1). Card holders are keyed by their
// source; a second card holder with a source already registered is refused,
// but a module may hold nodes and cards under one source. The server
// applies every card holder's answers once Init has run, and again whenever
// a module calls ReapplySchedulingHolds or ReapplySchedulingHoldsFor, which
// apply node and card holds in one pass.
func (h *Host) HoldCardScheduling(holder store.CardSchedulingHolder) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if err := h.st.AddCardSchedulingHolder(holder); err != nil {
		return fmt.Errorf("extension %s: %w", h.name, err)
	}
	h.reg.holders++
	return nil
}

// ReapplySchedulingHolds asks the server to apply every holder's answers to
// the whole fleet (store.ApplySchedulingHolds), node and card holders alike,
// instead of waiting for each node's next status report, up to a minute
// away. A module calls it when its answers change. It never blocks and may be called from any goroutine, at
// any time, except from inside a hook (SchedulingHolder.Hold,
// CardSchedulingHolder.HoldCard, a NodeEnrollmentGuard): a pass asks every holder again, so a holder that
// asked for one from Hold would keep the fleet in passes. Calls are
// coalesced: any number made while a pass is pending or running make at
// most one more pass, at least two seconds after the last one. The pass
// runs with the modules' services and stops with the server; a failed pass
// is logged and retried with a backoff. With no holder registered it does
// nothing.
func (h *Host) ReapplySchedulingHolds() { h.reg.holds.Request() }

// ReapplySchedulingHoldsFor is ReapplySchedulingHolds for a module that
// knows which nodes its answers changed for: the pass reads and writes
// those nodes and their cards (store.ApplySchedulingHoldsFor) rather than
// the fleet, so a
// module whose answers change often for a few nodes does not cost a fleet
// pass each time. It coalesces with every other request; requests naming
// more than holdpass.MaxScoped nodes between two passes make a fleet pass.
// With no nodes it does nothing.
func (h *Host) ReapplySchedulingHoldsFor(nodes []uint) { h.reg.holds.RequestNodes(nodes) }

// NodeFleet is every node's hold facts, ordered by id (enrollment order),
// read in one query on the read pool (store.NodeFleet). It is for a module
// that derives its answers from the whole fleet: at boot, and for a
// recompute after node lifecycle changes (store.NodeFleetTopic on the Bus,
// read with store.NodeFleetChangeOf), coalesced; never per status report or
// per event, and never from inside a hook. Subscribe before the first read,
// so no change between them is missed.
func (h *Host) NodeFleet(ctx context.Context) ([]store.NodeHoldFacts, error) {
	return h.st.NodeFleet(ctx)
}

// CardFleet is every card's hold facts (store.CardHoldFacts), ordered by
// node id and then card identity, read in one query on the read pool
// (store.CardFleet). It is NodeFleet for a card holder that derives its
// answers from the whole fleet's cards: a report that adds, removes or
// changes a card's hold facts publishes store.NodeFactsChanged on
// store.NodeFleetTopic, so the same subscription tells it when to read
// again, coalesced; never per status report or per event, and never from
// inside a hook.
func (h *Host) CardFleet(ctx context.Context) ([]store.CardHoldFacts, error) {
	return h.st.CardFleet(ctx)
}

// NodeIdentity is the salted hardware identity of each named node that
// exists, in ascending id order (store.NodeIdentity): the machine's system
// UUID and the serials of the cards the node owns, each as a 16-hex-digit
// HMAC-SHA256 keyed with salt plus the raw value's last four characters.
// Raw serials never leave the server. The digest differs per salt; the
// tail is the same for every caller. A node that does not exist is left
// out. ids holds 1 to 500 nodes and salt 1 to 256 bytes, else
// store.ErrNodeReadBatch or store.ErrIdentitySalt. It reads only the named
// nodes: call it for the nodes a page shows, not for the whole fleet on
// every event. Identity changes (a new host UUID, a swapped card) are not
// announced on store.NodeFleetTopic.
func (h *Host) NodeIdentity(ctx context.Context, ids []uint, salt []byte) ([]store.NodeIdentity, error) {
	return h.st.NodeIdentity(ctx, ids, salt)
}

// NodeInventory is one page of every node in enrollment order
// (store.NodeInventory): id, UUID, display name, enrollment time, whether
// its host UUID is known, and the cards it owns (index, model, whether each
// carries a serial), never a raw serial. cursor is 0 for the first page and
// the previous page's Next after that; limit is 1 to 500
// (store.ErrNodeReadBatch).
func (h *Host) NodeInventory(ctx context.Context, cursor uint, limit int) (store.NodeInventoryPage, error) {
	return h.st.NodeInventory(ctx, cursor, limit)
}

// HostIdentity is the salted identity of the machine this server runs on
// (hostidentity.Facts.Salted): its system UUID, digested as a node's host
// is so that under one salt it equals the host of each node on this
// machine, the board serial, the machine id and the root file system
// UUID, each a 16-hex-digit HMAC-SHA256 keyed with salt plus the raw
// value's last four characters, the zero part when unknown. On Linux the
// two root-only values come from the file systemd has root write before
// the server starts (hostidentity.RunFile). The facts are read once per
// process. salt is 1 to 256 bytes, else store.ErrIdentitySalt.
func (h *Host) HostIdentity(salt []byte) (hostidentity.Salted, error) {
	if len(salt) == 0 || len(salt) > store.MaxIdentitySalt {
		return hostidentity.Salted{}, store.ErrIdentitySalt
	}
	return hostidentity.Local().Salted(salt), nil
}

// NodeNamesByID is the display name of each named node that exists, keyed by
// id (store.NodeNamesByID): the name an operator gave it, otherwise its
// hostname. A node that does not exist is absent. No ids is an empty map
// without a read; more than 500 distinct ids is store.ErrNodeReadBatch. It
// is one read of the named nodes, for labelling a few nodes without paging
// NodeInventory.
func (h *Host) NodeNamesByID(ctx context.Context, ids []uint) (map[uint]string, error) {
	return h.st.NodeNamesByID(ctx, ids)
}

func (h *Host) mustBeOpen(method string) {
	if h.reg.closed {
		panic(fmt.Sprintf("extension %s: %s: %v", h.name, method, ErrHostClosed))
	}
}
