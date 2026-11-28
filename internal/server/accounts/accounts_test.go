package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func code(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	return ""
}

// A source belongs to the first owner that declares it: another owner's claim
// is refused with the source named and claims nothing, the same owner may
// declare again, and a name outside the grammar is refused.
func TestARegistryGivesEachSourceOneOwner(t *testing.T) {
	r := NewRegistry(nil, "")
	if r.provision != serverconfig.ProvisionAuto {
		t.Fatalf("default provision = %q", r.provision)
	}
	if err := r.Declare("acme_sso", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := r.Declare("acme_sso", "acme"); err != nil {
		t.Fatalf("declaring its own source again = %v", err)
	}
	var pe *messages.ProductError
	err := r.Declare("other", "beta", "acme")
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorIdentitySourceConflict || pe.Params()["source"] != "acme" {
		t.Fatalf("claiming another owner's source = %v", err)
	}
	if r.Owns("other", "beta") {
		t.Fatal("a refused claim claimed the source declared before the conflict")
	}
	if err := r.Declare("other", "Bad-Name"); err == nil || code(err) != "" {
		t.Fatalf("a source outside the grammar = %v", err)
	}
	if !r.Owns("acme_sso", "acme") || r.Owns("other", "acme") {
		t.Fatal("ownership is not the first claim's")
	}
}

// Every operation refuses a source its owner did not declare before it reads
// or writes anything.
func TestAccountsRefuseAnotherOwnersSource(t *testing.T) {
	st := &fakeStore{}
	r := NewRegistry(st, "")
	if err := r.Declare("first", "acme"); err != nil {
		t.Fatal(err)
	}
	a := r.For("second")
	calls := map[string]func() error{
		"sign-in": func() error {
			_, err := a.SignIn("acme", "s", Profile{Username: "u"}, strings.Repeat("a", 64))
			return err
		},
		"upsert":  func() error { _, err := a.Upsert("acme", "s", Profile{Username: "u"}); return err },
		"disable": func() error { return a.Disable("acme", "s") },
		"enable":  func() error { return a.Enable("acme", "s") },
		"revoke":  func() error { _, err := a.RevokeSessions("acme", "s"); return err },
		"list":    func() error { _, _, err := a.List("acme", 0, 10); return err },
	}
	for name, call := range calls {
		if got := code(call()); got != messages.ErrorIdentitySourceConflict {
			t.Fatalf("%s on another owner's source = %q", name, got)
		}
	}
	if len(st.calls) != 0 {
		t.Fatalf("the store was asked %v", st.calls)
	}
}

// A sign-in needs a handle the core minted and a username without
// surrounding spaces; both are refused before the store is asked.
func TestASignInChecksItsHandleAndProfileFirst(t *testing.T) {
	r := NewRegistry(nil, "")
	if err := r.Declare("first", "acme"); err != nil {
		t.Fatal(err)
	}
	a := r.For("first")
	for _, h := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := a.SignIn("acme", "s", Profile{Username: "u"}, h); code(err) != messages.ErrorIdentityTicketInvalid {
			t.Fatalf("handle %q = %v", h, err)
		}
	}
	for _, p := range []Profile{{}, {Username: " u"}, {Username: strings.Repeat("x", 65)}, {Username: "u", Email: strings.Repeat("e", 255)}} {
		if _, err := a.SignIn("acme", "s", p, strings.Repeat("a", 64)); code(err) != messages.ErrorExternalIdentityInvalid {
			t.Fatalf("profile %+v = %v", p, err)
		}
	}
	if _, _, err := a.List("acme", 0, MaxPage+1); err == nil {
		t.Fatal("a page above MaxPage was accepted")
	}
}

// fakeStore answers from linked (by subject) and records the calls it got;
// err, when set, is every write's answer.
type fakeStore struct {
	linked map[string]User
	err    error
	calls  []string
}

func (f *fakeStore) LinkedAccount(_, subject string) (User, uint, error) {
	f.calls = append(f.calls, "linked")
	u, ok := f.linked[subject]
	if !ok {
		return User{}, 0, ErrNotLinked
	}
	return u, u.ID + 100, nil
}

func (f *fakeStore) CreateExternalUser(username, _, _ string) (User, uint, error) {
	f.calls = append(f.calls, "create")
	if f.err != nil {
		return User{}, 0, f.err
	}
	return User{ID: 9, Username: username}, 109, nil
}

func (f *fakeStore) RenameLinkedAccount(_, subject, username string) (User, error) {
	f.calls = append(f.calls, "rename")
	if f.err != nil {
		return User{}, f.err
	}
	u := f.linked[subject]
	u.Username = username
	return u, nil
}

func (f *fakeStore) DisableLinkedAccount(string, string) error {
	f.calls = append(f.calls, "disable")
	return f.err
}

func (f *fakeStore) EnableLinkedAccount(string, string) error {
	f.calls = append(f.calls, "enable")
	return f.err
}

func (f *fakeStore) RevokeLinkedAccountSessions(string, string) (int, error) {
	f.calls = append(f.calls, "revoke")
	return 0, f.err
}

func (f *fakeStore) AccountsBySource(string, uint, int) ([]Account, uint, error) {
	f.calls = append(f.calls, "list")
	return nil, 0, f.err
}

func (f *fakeStore) IssueSignInTicket(_, _ uint, _, _ string, ttl time.Duration) (string, error) {
	f.calls = append(f.calls, "ticket")
	if ttl != TicketTTL {
		return "", errors.New("wrong lifetime")
	}
	return "ticket", nil
}

// A sign-in signs the linked account in without renaming it, creates an
// account for an unlinked identity only when the site provisions accounts,
// refuses a disabled account, and passes a store refusal on unchanged.
func TestASignInFindsOrCreatesTheAccountAndNeverRenamesIt(t *testing.T) {
	handle := strings.Repeat("a", 64)
	st := &fakeStore{linked: map[string]User{"corp:1": {ID: 1, Username: "alice"}, "corp:2": {ID: 2, Username: "bob", Disabled: true}}}
	r := NewRegistry(st, serverconfig.ProvisionAuto)
	if err := r.Declare("m", "acme"); err != nil {
		t.Fatal(err)
	}
	a := r.For("m")
	if _, err := a.SignIn("acme", "corp:1", Profile{Username: "alice.renamed"}, handle); err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.calls, ",") != "linked,ticket" {
		t.Fatalf("a linked sign-in asked the store %v, want no rename", st.calls)
	}
	st.calls = nil
	if _, err := a.SignIn("acme", "corp:new", Profile{Username: "carol"}, handle); err != nil || strings.Join(st.calls, ",") != "linked,create,ticket" {
		t.Fatalf("an unlinked sign-in under auto = %v, calls %v", err, st.calls)
	}
	st.calls = nil
	if _, err := a.SignIn("acme", "corp:2", Profile{Username: "bob"}, handle); code(err) != messages.ErrorUserDisabled || strings.Join(st.calls, ",") != "linked" {
		t.Fatalf("a disabled account's sign-in = %v, calls %v", err, st.calls)
	}

	refusal := messages.NewProductError(messages.ErrorIdentityUsernameTaken, "taken", map[string]any{"username": "carol"})
	st.err, st.calls = refusal, nil
	if _, err := a.SignIn("acme", "corp:new2", Profile{Username: "carol"}, handle); !errors.Is(err, refusal) {
		t.Fatalf("a store refusal became %v", err)
	}

	existing := NewRegistry(&fakeStore{}, serverconfig.ProvisionExisting)
	if err := existing.Declare("m", "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := existing.For("m").SignIn("acme", "corp:new", Profile{Username: "carol"}, handle); code(err) != messages.ErrorIdentityNotProvisioned {
		t.Fatalf("an unlinked sign-in under existing = %v", err)
	}
}

// A directory sync creates the account of an unlinked member whatever the
// provisioning policy, renames a linked one only when the name changed, and
// passes the store's refusal of an administrator on.
func TestAnUpsertCreatesOrRenames(t *testing.T) {
	st := &fakeStore{linked: map[string]User{"corp:1": {ID: 1, Username: "alice"}, "boss": {ID: 2, Username: "admin", Admin: true}}}
	r := NewRegistry(st, serverconfig.ProvisionExisting)
	if err := r.Declare("m", "acme"); err != nil {
		t.Fatal(err)
	}
	a := r.For("m")
	if got, err := a.Upsert("acme", "corp:new", Profile{Username: "dave"}); err != nil || got.Username != "dave" || strings.Join(st.calls, ",") != "linked,create" {
		t.Fatalf("upsert of an unlinked member = %+v %v, calls %v", got, err, st.calls)
	}
	st.calls = nil
	if _, err := a.Upsert("acme", "corp:1", Profile{Username: "alice"}); err != nil || strings.Join(st.calls, ",") != "linked" {
		t.Fatalf("upsert without a change = %v, calls %v", err, st.calls)
	}
	st.calls = nil
	if got, err := a.Upsert("acme", "corp:1", Profile{Username: "alice.a"}); err != nil || got.Username != "alice.a" || strings.Join(st.calls, ",") != "linked,rename" {
		t.Fatalf("upsert with a new name = %+v %v, calls %v", got, err, st.calls)
	}
	if got, err := a.Upsert("acme", "boss", Profile{Username: "admin"}); err != nil || !got.Protected {
		t.Fatalf("an administrator's unchanged upsert = %+v %v", got, err)
	}
	protected := messages.NewProductError(messages.ErrorIdentityProtectedUser, "protected", nil)
	st.err = protected
	if _, err := a.Upsert("acme", "boss", Profile{Username: "renamed"}); !errors.Is(err, protected) {
		t.Fatalf("renaming an administrator = %v", err)
	}
}
