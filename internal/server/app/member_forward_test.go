//go:build linux && dqlite

package app

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/storagetier"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// forwardCluster is a set of members joined by a real mutual-TLS member
// route on loopback. Each member's business handler names the member, so a
// test sees which process served a request.
type forwardCluster struct {
	t       *testing.T
	ca      *x509.Certificate
	caKey   *ecdsa.PrivateKey
	pool    *x509.CertPool
	members map[uint64]*forwardMember
}

// refused is how often the cluster's members refused business for reason.
func (c *forwardCluster) refused(reason string) uint64 {
	var n uint64
	for _, m := range c.members {
		n += m.forward.refusals.snapshot()[reason]
	}
	return n
}

type forwardMember struct {
	id         uint64
	forward    *memberForward
	public     *httptest.Server
	route      net.Listener
	cert       tls.Certificate
	leader     atomic.Uint64 // what this member observes; 0 is "no answer"
	sessions   atomic.Int64
	dropped    atomic.Int64
	authorized atomic.Int64
	admit      atomic.Bool
	// unanswered makes the next authorizations end as a grant that brought
	// no answer, as during a storage stall.
	unanswered atomic.Int64
	seen       chan *http.Request
}

func newForwardCluster(t *testing.T, ids ...uint64) *forwardCluster {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "member route test authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	c := &forwardCluster{t: t, ca: ca, caKey: key, pool: x509.NewCertPool(), members: map[uint64]*forwardMember{}}
	c.pool.AddCert(ca)
	for _, id := range ids {
		c.members[id] = c.newMember(id)
	}
	for _, member := range c.members {
		for _, peer := range c.members {
			member.forward.SetMemberRoute(peer.id, peer.route.Addr().String())
		}
		go func() { _ = member.forward.ServeMemberSessions(member.route) }()
	}
	t.Cleanup(func() {
		for _, member := range c.members {
			_ = member.forward.CloseMemberSessions(context.Background())
			member.public.Close()
		}
	})
	return c
}

func (c *forwardCluster) certificate(name string, serial int64) tls.Certificate {
	c.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		c.t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.ca, &key.PublicKey, c.caKey)
	if err != nil {
		c.t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (c *forwardCluster) newMember(id uint64) *forwardMember {
	c.t.Helper()
	m := &forwardMember{id: id, cert: c.certificate(fmt.Sprintf("member-%d", id), int64(id)+10), seen: make(chan *http.Request, 16)}
	m.admit.Store(true)
	m.forward = newMemberForward(nil, newBusinessRefusals(),
		func(ctx context.Context) (uint64, error) {
			if leader := m.leader.Load(); leader != 0 {
				return leader, nil
			}
			return 0, errors.New("no leader observed")
		},
		func() int { return int(m.sessions.Load()) },
		func() { m.dropped.Add(1); m.sessions.Store(0) })
	business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case m.seen <- r:
		default:
		}
		switch r.URL.Path {
		case "/stream":
			flusher := w.(http.Flusher)
			fmt.Fprintf(w, "first from %d\n", id)
			flusher.Flush()
			<-r.Context().Done()
		case "/upgrade":
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: member-test\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			fmt.Fprintf(rw, "member %d echoes %s", id, line)
			_ = rw.Flush()
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case "/die":
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		default:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Served-By", fmt.Sprint(id))
			fmt.Fprintf(w, "member=%d remote=%s host=%s tls=%t body=%s spoof=%q", id, r.RemoteAddr, r.Host, r.TLS != nil, body, r.Header.Get(memberForwardRemoteHeader))
		}
	})
	m.forward.local = business
	server := &tls.Config{Certificates: []tls.Certificate{m.cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: c.pool, MinVersion: tls.VersionTLS13}
	peerTLS := func(context.Context, uint64, string) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{m.cert}, RootCAs: c.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}, nil
	}
	authorize := func(context.Context, *x509.Certificate) error {
		m.authorized.Add(1)
		if m.unanswered.Load() > 0 {
			m.unanswered.Add(-1)
			return fmt.Errorf("grant: %w", context.DeadlineExceeded)
		}
		if !m.admit.Load() {
			return errors.New("not admitted")
		}
		return nil
	}
	if err := m.forward.SetMemberSecureTransport(id, server, peerTLS, authorize); err != nil {
		c.t.Fatal(err)
	}
	route, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	m.route = route
	m.public = httptest.NewServer(m.forward.wrap(business))
	return m
}

// lead makes every member observe leader, and waits until each acts on it.
func (c *forwardCluster) lead(leader uint64) {
	c.t.Helper()
	for _, member := range c.members {
		member.leader.Store(leader)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, member := range c.members {
		for {
			if observed, known := member.forward.observedLeader(); known && observed == leader {
				break
			}
			if time.Now().After(deadline) {
				c.t.Fatalf("member %d never observed leader %d", member.id, leader)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func forwardGet(t *testing.T, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestMemberForwardsBusinessToTheLeaderAsTheClientSentIt(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	c.lead(2)
	// The leader serves its own requests itself.
	if status, body, _ := forwardGet(t, c.members[2].public.URL+"/api/v1/nodes"); status != 200 || !strings.HasPrefix(body, "member=2 ") {
		t.Fatalf("leader did not serve locally: %d %q", status, body)
	}
	// Any other member hands the request to the leader. The leader sees the
	// client's address and Host, no TLS, and nothing the client claimed
	// about itself in the member route's own headers.
	follower := c.members[1].public
	req, _ := http.NewRequest(http.MethodPost, follower.URL+"/api/v1/settings/x", strings.NewReader("payload"))
	req.Host = "console.example:8816"
	req.Header.Set(memberForwardRemoteHeader, "203.0.113.9:1")
	req.Header.Set(memberForwardTLSHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got := string(body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Served-By") != "2" {
		t.Fatalf("follower answer: %d served-by=%q %q", resp.StatusCode, resp.Header.Get("X-Served-By"), got)
	}
	for _, want := range []string{"member=2 ", "remote=127.0.0.1:", "host=console.example:8816", "tls=false", "body=payload", `spoof=""`} {
		if !strings.Contains(got, want) {
			t.Fatalf("forwarded request lost %q: %q", want, got)
		}
	}
	// A probe asks about the member that received it.
	for _, path := range []string{"/healthz", "/api/v1/healthz", "/api/v1/readyz"} {
		if _, body, _ := forwardGet(t, follower.URL+path); !strings.HasPrefix(body, "member=1 ") {
			t.Fatalf("probe %s left the member: %q", path, body)
		}
	}
	// The membership check is paid per connection, not per request.
	for range 5 {
		forwardGet(t, follower.URL+"/again")
	}
	if n := c.members[2].authorized.Load(); n < 1 || n > 2 {
		t.Fatalf("leader authorized the forwarding member %d times for kept-alive requests", n)
	}
}

func TestMemberForwardCarriesStreamsAndUpgrades(t *testing.T) {
	c := newForwardCluster(t, 1, 2)
	c.lead(2)
	// An event stream reaches the client while the handler is still running.
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.members[1].public.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	cancel()
	resp.Body.Close()
	if err != nil || line != "first from 2\n" {
		t.Fatalf("stream through a follower: %q %v", line, err)
	}
	// An upgraded connection (the agent session, a terminal) is spliced to
	// the leader in both directions.
	conn, err := net.Dial("tcp", strings.TrimPrefix(c.members[1].public.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /upgrade HTTP/1.1\r\nHost: entry\r\nConnection: Upgrade\r\nUpgrade: member-test\r\n\r\n")
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("upgrade through a follower answered %q", status)
	}
	for {
		header, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if header == "\r\n" {
			break
		}
	}
	fmt.Fprintf(conn, "ping\n")
	if echo, err := reader.ReadString('\n'); err != nil || echo != "member 2 echoes ping\n" {
		t.Fatalf("upgraded connection: %q %v", echo, err)
	}
}

func TestMemberForwardRefusesInsteadOfGuessing(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	refused := func(name, url, reason string) {
		t.Helper()
		before := c.refused(reason)
		status, body, header := forwardGet(t, url)
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "member business service is not ready") {
			t.Fatalf("%s: %d %q", name, status, body)
		}
		if got := header.Get(businessRefusalHeader); got != reason {
			t.Fatalf("%s: refusal header %q, want %q", name, got, reason)
		}
		if got := c.refused(reason); got != before+1 {
			t.Fatalf("%s: %s counted %d -> %d, want one more", name, reason, before, got)
		}
	}
	// No member has observed a leader: nobody serves business.
	refused("no leader observed", c.members[1].public.URL+"/api/v1/nodes", refusalNoKnownLeader)
	c.lead(2)
	// The observation ages out when the database stops answering.
	c.members[1].leader.Store(0)
	c.members[1].forward.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-memberLeaderFresh - time.Second)})
	refused("stale observation", c.members[1].public.URL+"/api/v1/nodes", refusalNoKnownLeader)

	// Members disagree for a moment after a leader change: 1 still names 2,
	// while 2 already sees 3 lead. Member 2 refuses; it does not forward a
	// forwarded request, so the request never loops and never reaches 3.
	c.members[1].leader.Store(2)
	c.members[1].forward.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now()})
	c.members[2].leader.Store(3)
	c.members[2].forward.observed.Store(&memberLeaderObservation{leader: 3, at: time.Now()})
	for len(c.members[3].seen) > 0 {
		<-c.members[3].seen
	}
	refused("forwarded to a member that no longer leads", c.members[1].public.URL+"/api/v1/nodes", refusalNotTheLeader)
	if len(c.members[3].seen) != 0 || len(c.members[2].seen) != 0 {
		t.Fatal("a forwarded request was served or forwarded again by a member that does not lead")
	}

	// A member the leader does not admit is refused before business runs.
	c.lead(3)
	c.members[3].admit.Store(false)
	notAdmitted := c.refused(refusalPeerNotAdmitted)
	status, body, header := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes")
	if status != http.StatusForbidden || !strings.Contains(body, "member is not admitted") {
		t.Fatalf("unadmitted member was answered %d %q", status, body)
	}
	if got := header.Get(businessRefusalHeader); got != refusalPeerNotAdmitted {
		t.Fatalf("refusal header %q, want %q", got, refusalPeerNotAdmitted)
	}
	if got := c.refused(refusalPeerNotAdmitted); got != notAdmitted+1 {
		t.Fatalf("%s counted %d -> %d, want one more", refusalPeerNotAdmitted, notAdmitted, got)
	}
	if len(c.members[3].seen) != 0 {
		t.Fatal("business ran for a member that is not admitted")
	}
}

func TestMemberForwardSurvivesTheLeaderDyingMidRequest(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	c.lead(2)
	// The leader drops the connection while answering: the client gets a
	// refusal it may retry, not a hang and not another member's answer.
	unreachable := c.refused(refusalLeaderUnreachable)
	if status, _, header := forwardGet(t, c.members[1].public.URL+"/die"); status != http.StatusServiceUnavailable || header.Get(businessRefusalHeader) != refusalLeaderUnreachable {
		t.Fatalf("a request the leader dropped was answered %d, refusal %q", status, header.Get(businessRefusalHeader))
	}
	if got := c.refused(refusalLeaderUnreachable); got != unreachable+1 {
		t.Fatalf("%s counted %d -> %d, want one more", refusalLeaderUnreachable, unreachable, got)
	}
	// A client that goes away while the leader is answering says nothing
	// about the leader: it is not counted and opens no run of refusals.
	before := c.refused(refusalLeaderUnreachable)
	gone, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(gone, http.MethodGet, c.members[1].public.URL+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the cancelled request was answered")
	}
	time.Sleep(200 * time.Millisecond)
	if got := c.refused(refusalLeaderUnreachable); got != before {
		t.Fatalf("a client abort was counted as %s: %d -> %d", refusalLeaderUnreachable, before, got)
	}
	// The leader's route goes away entirely; requests are refused until the
	// members observe the next leader, then served by it.
	_ = c.members[2].forward.CloseMemberSessions(context.Background())
	if status, _, _ := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes"); status != http.StatusServiceUnavailable {
		t.Fatalf("request to a dead leader was answered %d", status)
	}
	c.members[1].leader.Store(3)
	c.members[3].leader.Store(3)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, body, _ := forwardGet(t, c.members[1].public.URL+"/api/v1/nodes")
		if status == 200 && strings.HasPrefix(body, "member=3 ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("requests did not follow the new leader: %d %q", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMemberThatDoesNotLeadClosesItsAgentSessions(t *testing.T) {
	c := newForwardCluster(t, 1, 2)
	c.members[1].sessions.Store(3)
	c.members[2].sessions.Store(4)
	c.lead(2)
	deadline := time.Now().Add(5 * time.Second)
	for c.members[1].dropped.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a member that does not lead kept its agent sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.members[2].dropped.Load() != 0 || c.members[2].sessions.Load() != 4 {
		t.Fatal("the leader closed its own agent sessions")
	}
	// No answer from the database is not a verdict: sessions stay.
	c.members[2].leader.Store(0)
	time.Sleep(3 * memberLeaderObserveInterval)
	if c.members[2].dropped.Load() != 0 {
		t.Fatal("sessions were closed although no other leader was observed")
	}
	// A session that lands here later (the entry does not know the leader)
	// is closed on the next observation too.
	c.members[1].sessions.Store(1)
	before := c.members[1].dropped.Load()
	deadline = time.Now().Add(5 * time.Second)
	for c.members[1].dropped.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("a later session on a member that does not lead was kept")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMemberForwardIsInertWithoutARoute(t *testing.T) {
	// A single server, or a store that is not a native cluster, never
	// configures the secure route: every request is served locally.
	served := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "local") })
	for name, configure := range map[string]func(*memberForward){
		"never configured": func(*memberForward) {},
		"identity only":    func(f *memberForward) { f.SetMemberIdentity(7, []byte("key")) },
	} {
		forward := newMemberForward(nil, nil, func(context.Context) (uint64, error) { return 9, nil }, nil, nil)
		configure(forward)
		public := httptest.NewServer(forward.wrap(served))
		if status, body, _ := forwardGet(t, public.URL+"/api/v1/nodes"); status != 200 || body != "local" {
			t.Fatalf("%s: %d %q", name, status, body)
		}
		public.Close()
		if err := forward.CloseMemberSessions(context.Background()); err != nil {
			t.Fatalf("%s: close: %v", name, err)
		}
	}
	forward := newMemberForward(nil, nil, nil, nil, nil)
	if err := forward.SetMemberSecureTransport(0, nil, nil, nil); err == nil {
		t.Fatal("an incomplete secure transport was accepted")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := forward.ServeMemberSessions(ln); err == nil {
		t.Fatal("an unconfigured route was served")
	}
}

// One member asks another's module a question over the member route and
// gets that member's own answer, whoever leads; a member that is not
// admitted, a module that answers nothing, and a member with no route are
// all "no answer", and nothing is served to a caller without a certificate.
func TestMemberAsksAnotherMembersModule(t *testing.T) {
	c := newForwardCluster(t, 1, 2, 3)
	for id, m := range c.members {
		m.forward.answers = func(name string) MemberAnswerer {
			if name != "fake" {
				return nil
			}
			return func(_ context.Context, question []byte) ([]byte, error) {
				if string(question) == "fail" {
					return nil, errors.New("no")
				}
				return []byte(fmt.Sprintf("member %d heard %q", id, question)), nil
			}
		}
	}
	ask := func(from, to uint64, name, question string) (string, error) {
		out, err := c.members[from].forward.ask(context.Background(), to, name, []byte(question))
		return string(out), err
	}
	// No leader is observed and business is refused; a question is still answered.
	for _, to := range []uint64{2, 3} {
		if got, err := ask(1, to, "fake", "hello"); err != nil || got != fmt.Sprintf(`member %d heard "hello"`, to) {
			t.Fatalf("ask member %d = %q, %v", to, got, err)
		}
	}
	c.lead(2)
	if got, err := ask(3, 1, "fake", "again"); err != nil || got != `member 1 heard "again"` {
		t.Fatalf("ask a member that does not lead = %q, %v", got, err)
	}
	for name, call := range map[string]func() (string, error){
		"a module that answers nothing": func() (string, error) { return ask(1, 2, "other", "x") },
		"a name that is no module":      func() (string, error) { return ask(1, 2, "../fake", "x") },
		"an answerer that fails":        func() (string, error) { return ask(1, 2, "fake", "fail") },
		"a member with no route":        func() (string, error) { return ask(1, 9, "fake", "x") },
	} {
		if got, err := call(); !errors.Is(err, ErrMemberUnreachable) {
			t.Errorf("%s = %q, %v, want %v", name, got, err, ErrMemberUnreachable)
		}
	}
	if _, err := ask(1, 2, "fake", strings.Repeat("x", MemberQuestionLimit+1)); err == nil {
		t.Error("a question over the limit was sent")
	}
	// The member asked does not admit the asker.
	c.members[2].admit.Store(false)
	c.members[3].forward.proxies = map[uint64]*httputil.ReverseProxy{} // a new connection, so admission is checked again
	if got, err := ask(3, 2, "fake", "x"); !errors.Is(err, ErrMemberUnreachable) {
		t.Errorf("asked by a member that is not admitted = %q, %v", got, err)
	}
	// Without a member certificate the route does not complete a handshake.
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: c.pool, ServerName: "127.0.0.1"}}, Timeout: 5 * time.Second}
	if resp, err := plain.Post("https://"+c.members[1].route.Addr().String()+memberAskPrefix+"fake", "application/octet-stream", strings.NewReader("x")); err == nil {
		resp.Body.Close()
		t.Errorf("a caller without a member certificate was answered %d", resp.StatusCode)
	}
}

// A module's Init runs before the member's route exists. A question asked
// then says so, apart from a single server and from a member that does not
// answer, so the module can tell "not asked" from "no answer".
func TestAskingBeforeTheMemberRouteIsUp(t *testing.T) {
	ctx := context.Background()
	single := &App{}
	if _, err := single.askMember(ctx, 2, "fake", nil); !errors.Is(err, ErrNotClusterMember) {
		t.Fatalf("a server on an ordinary store = %v", err)
	}
	never := &App{ha: &memberRuntime{}}
	if _, err := never.askMember(ctx, 2, "fake", nil); !errors.Is(err, ErrNotClusterMember) {
		t.Fatalf("a member never enrolled in a cluster = %v", err)
	}
	// Enrolled, and the modules' Init is running: no route yet.
	runtime := &memberRuntime{bundle: &enrollmentBundle{}}
	member := &App{ha: runtime}
	if _, err := member.askMember(ctx, 2, "fake", nil); !errors.Is(err, ErrMemberRouteNotReady) {
		t.Fatalf("before the route exists = %v, want %v", err, ErrMemberRouteNotReady)
	}
	// The route exists but has no certificate-bound transport yet.
	runtime.forward = newMemberForward(nil, nil, func(context.Context) (uint64, error) { return 0, errors.New("none") }, func() int { return 0 }, func() {})
	if _, err := member.askMember(ctx, 2, "fake", nil); !errors.Is(err, ErrMemberRouteNotReady) {
		t.Fatalf("before the transport is set = %v, want %v", err, ErrMemberRouteNotReady)
	}
	// Once it is up, a member that is not there is "no answer".
	c := newForwardCluster(t, 1, 2)
	up := &App{ha: &memberRuntime{bundle: &enrollmentBundle{}, forward: c.members[1].forward}}
	if _, err := up.askMember(ctx, 9, "fake", nil); !errors.Is(err, ErrMemberUnreachable) {
		t.Fatalf("a member with no route = %v, want %v", err, ErrMemberUnreachable)
	}
}

// The order New starts a cluster member in, with the functions it calls: the
// modules' Init first (startExtensions), then the member's handler, which
// makes the route to the other members, then the certificate-bound
// transport. A module that asks a member from Init is told the route is not
// ready; the same Host gets the member's answer once the route is up. No
// database is started: the store and the hub are only held by the route.
func TestAModuleAsksAMemberInTheOrderNewStartsIt(t *testing.T) {
	ctx := context.Background()
	c := newForwardCluster(t, 2)
	c.members[2].forward.answers = func(name string) MemberAnswerer {
		return func(_ context.Context, question []byte) ([]byte, error) {
			return []byte(name + " on member 2 heard " + string(question)), nil
		}
	}
	runtime := &memberRuntime{bundle: &enrollmentBundle{}}
	a := &App{ha: runtime, store: &store.Store{}, hub: &hub.Hub{}}
	runtime.business = a
	host := &Host{app: a, name: "fake"}

	// 1. Init runs: no route exists.
	if _, err := host.AskMember(ctx, 2, []byte("at init")); !errors.Is(err, ErrMemberRouteNotReady) {
		t.Fatalf("asked from Init = %v, want %v", err, ErrMemberRouteNotReady)
	}
	// 2. New wraps the handler: the route exists, without its transport.
	_ = a.memberHandler(http.NotFoundHandler())
	if runtime.forward == nil {
		t.Fatal("the member's handler made no route")
	}
	if _, err := host.AskMember(ctx, 2, []byte("no transport")); !errors.Is(err, ErrMemberRouteNotReady) {
		t.Fatalf("asked before the transport = %v, want %v", err, ErrMemberRouteNotReady)
	}
	// 3. The member's sessions are routed: certificates and the peers' routes.
	cert := c.certificate("member-7", 77)
	server := &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: c.pool, MinVersion: tls.VersionTLS13}
	peerTLS := func(context.Context, uint64, string) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: c.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}, nil
	}
	if err := runtime.forward.SetMemberSecureTransport(7, server, peerTLS, func(context.Context, *x509.Certificate) error { return nil }); err != nil {
		t.Fatal(err)
	}
	runtime.forward.SetMemberRoute(2, c.members[2].route.Addr().String())
	got, err := host.AskMember(ctx, 2, []byte("after the route is up"))
	if err != nil || string(got) != "fake on member 2 heard after the route is up" {
		t.Fatalf("asked after the route is up = %q, %v", got, err)
	}
}

// Seen on the real hosts: during a storage stall the leader's grant for a
// member's connection timed out, the result was kept for the life of that
// connection, and for 62 to 102 seconds every request the member forwarded
// was answered 403 "member is not admitted" although the member was admitted
// all along. A grant that brings no answer decides nothing: the request is
// answered "not ready", and the next request on the same connection asks
// again and is served.
func TestGrantTimeoutDuringAStallDoesNotRefuseTheMembersConnectionAfterwards(t *testing.T) {
	c := newForwardCluster(t, 1, 2)
	c.lead(2)
	leader, follower := c.members[2], c.members[1]
	leader.unanswered.Store(2)
	for i := range 2 {
		status, body, header := forwardGet(t, follower.public.URL+"/api/v1/nodes")
		if status != http.StatusServiceUnavailable || !strings.Contains(body, "member business service is not ready") {
			t.Fatalf("request %d during the stall: %d %q, want 503 not ready, never 403", i, status, body)
		}
		if got := header.Get(businessRefusalHeader); got != refusalPeerUndecided {
			t.Fatalf("request %d during the stall: refusal %q, want %q", i, got, refusalPeerUndecided)
		}
	}
	status, body, header := forwardGet(t, follower.public.URL+"/api/v1/nodes")
	if status != http.StatusOK || header.Get("X-Served-By") != "2" {
		t.Fatalf("first request after the stall: %d %q", status, body)
	}
	// The stall's attempts and the one that answered: three questions, and
	// none after the answer, on the one connection the member keeps open.
	if got := leader.authorized.Load(); got != 3 {
		t.Fatalf("the leader asked %d times, want 3", got)
	}
	if status, _, _ := forwardGet(t, follower.public.URL+"/api/v1/nodes"); status != http.StatusOK || leader.authorized.Load() != 3 {
		t.Fatalf("a decided connection: status %d, asked %d times", status, leader.authorized.Load())
	}
	// A refusal is an answer, and is kept.
	c2 := newForwardCluster(t, 1, 2)
	c2.lead(2)
	c2.members[2].admit.Store(false)
	for range 2 {
		if status, _, _ := forwardGet(t, c2.members[1].public.URL+"/api/v1/nodes"); status != http.StatusForbidden {
			t.Fatalf("a member that is not admitted was answered %d", status)
		}
	}
	if got := c2.members[2].authorized.Load(); got != 1 {
		t.Fatalf("a refused connection was asked about %d times, want 1", got)
	}
}

// Each change of the leader a member sends business to leaves one line with
// the leader before and after, whether this member leads, how long the last
// observation was ago and how many agent sessions it closed; an unchanged
// leader leaves none.
func TestALeaderChangeLeavesOneBusinessLine(t *testing.T) {
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey || a.Key == "since_last_observation_ms" {
			return slog.Attr{}
		}
		return a
	}}))
	var leader atomic.Uint64
	held, dropped := 3, 0
	forward := newMemberForward(log, nil, func(context.Context) (uint64, error) { return leader.Load(), nil },
		func() int { return held }, func() { dropped++; held = 0 })
	forward.SetMemberIdentity(7, []byte("key"))
	ctx := context.Background()
	for _, id := range []uint64{7, 7, 9, 9, 0, 7} {
		leader.Store(id)
		forward.observeLeaderOnce(ctx)
	}
	want := `level=INFO msg="business follows a new database leader" new_leader=7 this_member=7 leads=true learned="leader observation" agent_sessions_closed=0
level=INFO msg="business follows a new database leader" new_leader=9 this_member=7 leads=false learned="leader observation" agent_sessions_closed=3 old_leader=7
level=INFO msg="closed agent sessions held by a member that does not lead" sessions=3 leader=9
level=INFO msg="business follows a new database leader" new_leader=7 this_member=7 leads=true learned="leader observation" agent_sessions_closed=0 old_leader=9
`
	if out.String() != want {
		t.Fatalf("logged\n%s\nwant\n%s", out.String(), want)
	}
	if dropped != 1 {
		t.Fatalf("sessions were closed %d times", dropped)
	}
}

// A leader observation is acted on for three seconds at the tiers' own
// timing and longer, in proportion, on a tier that elects later: at an
// election timeout of 3 s a leader's death takes up to two of them to a new
// leader, and three seconds of freshness would make every member refuse
// business on every leader change.
func TestALeaderObservationStaysFreshThroughAnElectionOfTheTier(t *testing.T) {
	at := func(name string) storagetier.Tier {
		tier, err := storagetier.Parse(name)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	previous := cluster.ProcessStorageTier()
	t.Cleanup(func() { cluster.UseStorageTier(previous) })
	// The member acts on what the tier of its own process allows.
	fresh := func(name string, age time.Duration) bool {
		cluster.UseStorageTier(at(name))
		f := &memberForward{}
		f.observed.Store(&memberLeaderObservation{leader: 2, at: time.Now().Add(-age)})
		leader, ok := f.observedLeader()
		return ok && leader == 2
	}
	for _, tc := range []struct {
		tier string
		age  time.Duration
		want bool
	}{
		{"standard", 2 * time.Second, true}, {"standard", 4 * time.Second, false},
		{"slow-storage", 2 * time.Second, true}, {"slow-storage", 4 * time.Second, false},
		{"slow-storage@100ms", 5 * time.Second, true}, {"slow-storage@100ms", 7 * time.Second, false},
		{"slow-storage@200ms", 8 * time.Second, true}, {"slow-storage@200ms", 13 * time.Second, false},
	} {
		if got := fresh(tc.tier, tc.age); got != tc.want {
			t.Errorf("%s: an observation %s old is acted on: %v, want %v", tc.tier, tc.age, got, tc.want)
		}
		// The window never ends before the tier's longest election can.
		tier := at(tc.tier)
		if _, longest := tier.ElectionTimer(); tier.Stretch(memberLeaderFresh) < longest+memberLeaderObserveInterval {
			t.Errorf("%s: an observation is fresh for %s, shorter than the tier's longest election timer %s and one observation", tc.tier, tier.Stretch(memberLeaderFresh), longest)
		}
	}
}
