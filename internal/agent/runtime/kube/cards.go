package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

// Borrowing cards from the kubelet.
//
// Every card on a node has exactly one allocator. Where a device plugin
// advertises the node's cards, that is the kubelet; the platform takes a card
// from it by running a hold pod that requests that one card, and gives it
// back by deleting the pod. A device plugin cannot be told which card to
// give, so borrowing takes every free card at once — then there is no choice
// left to make — finds out from the kubelet which hold pod got which card,
// and returns the ones it does not need. Instance pods request no device
// resource; they get their cards the way docker containers do, through the
// runtime class. A card stays borrowed while an instance uses it and for
// IdleReturn after the last one stopped, so a redeploy finds it again.
//
// Where no device plugin advertises the cards, the kubelet allocates none of
// them, and the platform simply uses them.

// allocationLister is the kubelet's PodResources List (podresources.Client).
type allocationLister interface {
	List(ctx context.Context) ([]podresources.Allocation, error)
}

type cardPool struct {
	// priorityClass is the hold pods' PriorityClass (Options.PriorityClass).
	priorityClass string
	api           *kubeapi.Client
	kubelet       allocationLister
	ns, node      string
	holdImage     string
	holdCmd       []string
	// pullSecrets are the agent pod's image pull secrets, for holdImage.
	pullSecrets []kubeapi.LocalRef
	owner       *kubeapi.OwnerReference
	// cards maps device index to card UUID, from the node's own probe.
	cards func(context.Context) (map[int]string, error)
	// cardBDFs maps device index to PCI address, for the device plugins that
	// name their devices that way (cardPool.cardOf); nil when the node has none.
	cardBDFs func(context.Context) (map[int]string, error)
	// idleReturn is how long an unused borrowed card is kept; negative keeps
	// it for good.
	idleReturn time.Duration
	now        func() time.Time
	log        *slog.Logger
	poll       time.Duration
	// releaseWait and borrowRetry bound the waits of cards_borrow.go, and
	// scheduleWait that of cards_sched.go; zero uses the defaults.
	releaseWait, borrowRetry, scheduleWait time.Duration
	// hamiScheduler is the name of HAMi's scheduler (DefaultHAMiScheduler
	// unless the cluster renamed it).
	hamiScheduler string
	// podsMu guards the cache of the node's pods (cards_holders.go).
	podsMu        sync.Mutex
	podsCache     []kubeapi.Pod
	podsAt        time.Time
	podsTTL       time.Duration
	podsForbidden bool

	// borrowMu serializes borrows. A borrow talks to the API server and the
	// kubelet for up to minutes, so it never runs under mu, which status
	// reporting (holders) and instance bookkeeping take.
	borrowMu sync.Mutex
	// mu guards everything below.
	mu sync.Mutex
	// view is how the node allocates its cards as of the last load
	// (cards_mode.go).
	view cardView
	// sig is the signature of the view the held and stale maps were read for.
	sig  string
	held map[string]string // card UUID -> hold pod name
	// stale are the cards held by hold pods created for another mode than the
	// node's now (cards_switch.go).
	stale     map[string]staleHold
	users     map[string]map[string]bool
	idleSince map[string]time.Time
	// starting are the instances that claimed cards and whose pod Run has not
	// created yet: they have no pod to find (see reapUsers).
	starting map[string]bool
	// returning are the hold pods this pool deleted and the kubelet still
	// lists; their cards are not held, whatever the allocations say.
	returning map[string]bool
	// pins are the cards in use outside any instance (holdForCheck), by how
	// many users; they are not returned meanwhile.
	pins   map[string]int
	loaded bool
}

var (
	deviceIndexSuffix = regexp.MustCompile(`-(\d+)$`)
	// ackDeviceID is ACK GPU sharing's device ID: one per GiB of a card's
	// memory, "gpu<card number>_mem_<n>".
	ackDeviceID = regexp.MustCompile(`^gpu(\d+)_mem_\d+$`)
)

// cardOf maps a device plugin's device ID to a card UUID: NVIDIA's are the
// UUIDs themselves, HAMi's are the UUID and a share number ("GPU-5b6f...-9"),
// ACK's name the card number, others end in the device index.
func cardOf(id string, byIndex map[int]string) string {
	for _, uuid := range byIndex {
		if uuid == id {
			return uuid
		}
	}
	if m := ackDeviceID.FindStringSubmatch(id); m != nil {
		n, _ := strconv.Atoi(m[1])
		return byIndex[n]
	}
	if m := deviceIndexSuffix.FindStringSubmatch(id); m != nil {
		base := strings.TrimSuffix(id, m[0])
		for _, uuid := range byIndex {
			if uuid == base {
				return uuid
			}
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			return byIndex[n]
		}
	}
	return ""
}

// pciAddress is a device ID that is a PCI address, with or without the
// domain.
var pciAddress = regexp.MustCompile(`^([0-9a-fA-F]{4,8}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// normalizePCI lower-cases a PCI address and drops the all-zero domain, so
// "0000:43:00.0" and "43:00.0" name the same function.
func normalizePCI(addr string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(addr)), "0000:")
}

// cardOf is the package's cardOf, which also knows the cards by PCI
// address: AMD's device plugin names each device by it ("0000:43:00.0").
func (p *cardPool) cardOf(ctx context.Context, id string, byIndex map[int]string) string {
	if card := cardOf(id, byIndex); card != "" {
		return card
	}
	if p.cardBDFs == nil || !pciAddress.MatchString(id) {
		return ""
	}
	bdfs, err := p.cardBDFs(ctx)
	if err != nil {
		p.log.Warn("cannot read the cards' PCI addresses to match a device plugin's devices", "err", err)
		return ""
	}
	want := normalizePCI(id)
	for index, bdf := range bdfs {
		if normalizePCI(bdf) == want {
			return byIndex[index]
		}
	}
	return ""
}

func randomSuffix() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// load classifies the node's cards and, the first time, rebuilds the pool
// from the hold pods and instance pods already on the node, so a restarted
// agent picks up where it left off. When the node's mode changed since, or
// hold pods of an earlier mode are still around, which cards the platform
// holds is read again (cards_switch.go).
func (p *cardPool) load(ctx context.Context) error {
	view, err := p.classify(ctx)
	if err != nil {
		return err
	}
	p.view = view
	sig := view.signature()
	if p.loaded {
		if sig != p.sig || len(p.stale) > 0 {
			if err := p.resync(ctx, false); err != nil {
				return err
			}
			p.sig = sig
			for card := range p.held {
				if _, idle := p.idleSince[card]; !idle && len(p.users[card]) == 0 {
					p.idleSince[card] = p.now()
				}
			}
		}
		return nil
	}
	p.held, p.users, p.idleSince = map[string]string{}, map[string]map[string]bool{}, map[string]time.Time{}
	p.starting, p.returning, p.stale = map[string]bool{}, map[string]bool{}, map[string]staleHold{}
	if err := p.resync(ctx, true); err != nil {
		return err
	}
	pods, err := p.instancePods(ctx)
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, card := range strings.Split(pod.Metadata.Annotations[AnnotationCards], ",") {
			if card != "" {
				p.use(card, pod.Metadata.Name)
			}
		}
	}
	for card := range p.held {
		if len(p.users[card]) == 0 {
			p.idleSince[card] = p.now()
		}
	}
	p.sig = sig
	p.loaded = true
	return nil
}

// instancePods lists the instance pods on this node.
func (p *cardPool) instancePods(ctx context.Context) (kubeapi.PodList, error) {
	var pods kubeapi.PodList
	q := kubeapi.ListQuery(LabelRole+"="+RoleInstance+","+LabelNode+"="+p.node, map[string]string{"spec.nodeName": p.node})
	if err := p.api.Get(ctx, kubeapi.PodsPath(p.ns)+q, &pods); err != nil {
		return pods, fmt.Errorf("list instance pods: %w", err)
	}
	return pods, nil
}

func (p *cardPool) use(card, instance string) {
	if p.users[card] == nil {
		p.users[card] = map[string]bool{}
	}
	p.users[card][instance] = true
	delete(p.idleSince, card)
}

// listHeld reads, from the kubelet's allocations, which hold pod of this
// node has which card. It touches no pool state, so it runs with or without
// mu.
func (p *cardPool) listHeld(ctx context.Context, resource string) (map[string]string, error) {
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return nil, err
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	held := map[string]string{}
	for _, a := range allocs {
		if a.Namespace != p.ns || !strings.HasPrefix(a.Pod, holdPrefix) || a.Resource != resource {
			continue
		}
		if card := p.cardOf(ctx, a.DeviceID, byIndex); card != "" {
			held[card] = a.Pod
		}
	}
	return held, nil
}

// setHeld records the kubelet's view of the borrowed cards, minus the hold
// pods this pool already deleted (the kubelet lists them until their
// containers are gone). mu is held.
func (p *cardPool) setHeld(seen map[string]string) {
	present := map[string]bool{}
	held := map[string]string{}
	for card, pod := range seen {
		present[pod] = true
		if !p.returning[pod] {
			held[card] = pod
		}
	}
	for pod := range p.returning {
		if !present[pod] {
			delete(p.returning, pod)
		}
	}
	p.held = held
}

const holdPrefix = "gpuworks-hold-"

// acquire makes the cards at indexes the platform's for instance and
// returns their UUIDs. Borrowing from the kubelet is slow, so the pool's
// state is not locked across it: the instance is registered as a user of its
// cards first (which keeps them from being returned meanwhile) and unregistered
// again if the borrow fails.
func (p *cardPool) acquire(ctx context.Context, instance string, indexes []int) ([]string, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	want, missing, err := p.claim(ctx, instance, indexes)
	if err != nil {
		return nil, err
	}
	if missing {
		if err := p.borrowWithRetry(ctx, want); err != nil {
			p.release(instance)
			return nil, err
		}
	}
	return want, nil
}

// claim registers instance as a user of the cards at indexes and reports
// their UUIDs and whether the platform still has to borrow some of them.
func (p *cardPool) claim(ctx context.Context, instance string, indexes []int) ([]string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, false, err
	}
	if p.view.shared != "" {
		return nil, false, errors.New(p.view.shared)
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, false, err
	}
	want := make([]string, 0, len(indexes))
	for _, i := range indexes {
		uuid := byIndex[i]
		if uuid == "" {
			if p.view.resource == "" {
				continue // nothing to borrow; the card is used by index
			}
			return nil, false, fmt.Errorf("card %d has no UUID on this node", i)
		}
		if p.isStale(uuid) {
			return nil, false, errors.New("card " + uuid + " cannot be used: " + p.staleReason(uuid))
		}
		want = append(want, uuid)
	}
	missing := false
	for _, card := range want {
		if p.view.resource != "" && p.held[card] == "" {
			missing = true
		}
		p.use(card, instance)
	}
	if len(want) > 0 {
		p.starting[instance] = true
	}
	return want, missing, nil
}

// settled says instance's pod now exists: from here on, a missing pod means
// the instance is gone (reapUsers).
func (p *cardPool) settled(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.starting, instance)
}

// release says instance no longer uses its cards.
func (p *cardPool) release(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseLocked(instance)
}

func (p *cardPool) releaseLocked(instance string) {
	delete(p.starting, instance)
	for card, users := range p.users {
		if users[instance] {
			delete(users, instance)
			if len(users) == 0 {
				delete(p.users, card)
				if p.held[card] != "" {
					p.idleSince[card] = p.now()
				}
			}
		}
	}
}

// reapUsers drops the instances whose pod no longer exists from the users
// of their cards. The engine calls release only through Remove, and skips it
// for a container Status says is gone, so a pod deleted by anything else (a
// kubectl delete, an eviction that removed it, a node drain) would keep its
// cards borrowed for good.
func (p *cardPool) reapUsers(ctx context.Context) {
	p.mu.Lock()
	if !p.loaded {
		p.mu.Unlock()
		return
	}
	// Instances still starting have no pod yet. They are skipped, and the
	// snapshot is taken before the list so that a pod created after the list
	// belongs to an instance that was starting at the snapshot.
	candidates := map[string]bool{}
	for _, users := range p.users {
		for instance := range users {
			if !p.starting[instance] {
				candidates[instance] = true
			}
		}
	}
	p.mu.Unlock()
	if len(candidates) == 0 {
		return
	}
	pods, err := p.instancePods(ctx)
	if err != nil {
		p.log.Warn("cannot list the instance pods to find vanished ones", "err", err)
		return
	}
	for _, pod := range pods.Items {
		delete(candidates, pod.Metadata.Name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for instance := range candidates {
		if p.starting[instance] {
			continue
		}
		p.log.Info("the pod of an instance is gone; releasing its cards", "instance", instance)
		p.releaseLocked(instance)
	}
}

// setIdleReturn replaces how long an unused borrowed card is kept and returns
// the previous value; the next sweep uses the new one.
func (p *cardPool) setIdleReturn(v time.Duration) (old time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old, p.idleReturn = p.idleReturn, v
	return old
}

func (p *cardPool) idleReturnSetting() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idleReturn
}

// sweep drops the users whose pod vanished and returns the cards that have
// been idle longer than idleReturn.
func (p *cardPool) sweep(ctx context.Context) {
	p.reapUsers(ctx)
	p.mu.Lock()
	if !p.loaded {
		p.mu.Unlock()
		return
	}
	// A hold pod made for another mode than the node's now is of no use to
	// the platform; it goes as soon as no instance runs on its card.
	var pods []string
	for card, st := range p.stale {
		if len(p.users[card]) > 0 || p.pins[card] > 0 || p.returning[st.pod] {
			continue
		}
		p.log.Info("returning a card held for the node's earlier mode", "card", card, "pod", st.pod)
		pods = append(pods, st.pod)
		p.returning[st.pod] = true
		delete(p.stale, card)
	}
	if p.idleReturn < 0 {
		p.mu.Unlock()
		p.deletePods(ctx, pods)
		return
	}
	for card, since := range p.idleSince {
		if len(p.users[card]) > 0 || p.pins[card] > 0 || p.now().Sub(since) < p.idleReturn {
			continue
		}
		if pod := p.held[card]; pod != "" {
			p.log.Info("returning an idle card to Kubernetes", "card", card, "pod", pod)
			pods = append(pods, pod)
			p.returning[pod] = true
		}
		delete(p.held, card)
		delete(p.idleSince, card)
	}
	p.mu.Unlock()
	p.deletePods(ctx, pods)
}

// keepAll borrows every free card for good (a cluster that keeps cards).
func (p *cardPool) keepAll(ctx context.Context) error {
	p.borrowMu.Lock()
	defer p.borrowMu.Unlock()
	p.mu.Lock()
	err := p.load(ctx)
	view := p.view
	skip := err != nil || view.resource == "" || view.shared != ""
	var all []string
	if !skip && (view.mode == modeHAMi || view.mode == modeACK) {
		byIndex, cerr := p.cards(ctx)
		if cerr != nil {
			p.mu.Unlock()
			return cerr
		}
		for _, uuid := range byIndex {
			if !p.isStale(uuid) {
				all = append(all, uuid)
			}
		}
		sort.Strings(all)
	}
	p.mu.Unlock()
	if skip {
		return err
	}
	if view.mode == modeHAMi || view.mode == modeACK {
		err = p.borrow(ctx, all)
	} else {
		_, _, err = p.takeAllFree(ctx)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for card := range p.held {
		if len(p.users[card]) == 0 {
			p.idleSince[card] = p.now()
		}
	}
	return nil
}
