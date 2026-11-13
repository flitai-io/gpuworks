package selfinstall

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Account is a local account's numeric identity: what the kernel compares a
// directory's owner and group against.
type Account struct {
	UID      string
	GroupIDs []string
}

// lookupAccount is the default behind Deps.LookupAccount. With CGO disabled
// os/user reads the passwd and group databases directly.
func lookupAccount(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, err
	}
	gids, err := u.GroupIds()
	if err != nil {
		gids = nil
	}
	return Account{UID: u.Uid, GroupIDs: append(gids, u.Gid)}, nil
}

// errUnsupportedStat marks a platform statDirAccess cannot inspect.
var errUnsupportedStat = errors.New("directory access inspection is unsupported on this platform")

// dirAccess is what a permission check needs to know about one directory.
type dirAccess struct {
	uid, gid uint32
	mode     os.FileMode
	// acl means an access ACL may grant more than the mode bits show.
	acl bool
}

// searchIdentity is the uid and groups the service runs with.
type searchIdentity struct {
	uid    uint32
	hasUID bool
	gids   map[uint32]bool
}

// canSearch applies the kernel's rule for the search (x) bit: the owner class
// alone decides for the owner, the group class for a member, otherwise other.
func (id searchIdentity) canSearch(d dirAccess) bool {
	switch {
	case id.hasUID && id.uid == d.uid:
		return d.mode&0o100 != 0
	case id.gids[d.gid]:
		return d.mode&0o010 != 0
	default:
		return d.mode&0o001 != 0
	}
}

// serviceIdentity resolves what the unit's User= and Group= give the process.
// An account the install is about to create has no uid yet; it will belong to
// the credential group only.
func (i *Installer) serviceIdentity(name string) (searchIdentity, error) {
	id := searchIdentity{gids: map[uint32]bool{}}
	lookup := i.deps.LookupAccount
	if lookup == nil {
		lookup = lookupAccount
	}
	account, err := lookup(name)
	var unknown user.UnknownUserError
	switch {
	case errors.As(err, &unknown):
	case err != nil:
		return searchIdentity{}, err
	default:
		uid, err := strconv.ParseUint(account.UID, 10, 32)
		if err != nil {
			return searchIdentity{}, fmt.Errorf("account %s has uid %q: %w", name, account.UID, err)
		}
		id.uid, id.hasUID = uint32(uid), true
		for _, gid := range account.GroupIDs {
			if n, err := strconv.ParseUint(gid, 10, 32); err == nil {
				id.gids[uint32(n)] = true
			}
		}
	}
	if i.deps.LookupGroup != nil {
		if group, err := i.deps.LookupGroup(DefaultCredentialGroup); err == nil {
			if n, err := strconv.ParseUint(group.Gid, 10, 32); err == nil {
				id.gids[uint32(n)] = true
			}
		}
	}
	return id, nil
}

// checkServiceUserReach fails when the service account cannot even reach the
// install tree: chownTree hands the account its own directories, but the
// directories above them stay as they are, and every one of those needs the
// search bit for the account. An install under /root (0700) is the classic
// case: systemd's ExecStartPre `test -x` then fails without a line of server
// log. Missing ancestors are left out; the install creates them traversable.
func (i *Installer) checkServiceUserReach(pf Preflight, roots []string) error {
	if pf.Mode != ModeSystemd || pf.ServiceUser == "" || pf.ServiceUser == "root" {
		return nil
	}
	id, err := i.serviceIdentity(pf.ServiceUser)
	if err != nil {
		return fmt.Errorf("resolve service account %s: %w", pf.ServiceUser, err)
	}
	checked := map[string]bool{}
	for _, root := range roots {
		for dir := filepath.Dir(root); !checked[dir]; dir = filepath.Dir(dir) {
			checked[dir] = true
			access, err := statDirAccess(dir)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, errUnsupportedStat) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect %s: %w", dir, err)
			}
			// An ACL can grant what the mode denies; without evaluating it,
			// a refusal could be wrong, so only certain denials refuse.
			if !id.canSearch(access) && !access.acl {
				return fmt.Errorf("the service account %s cannot enter %s (mode %s), so systemd could not start %s; "+
					"install somewhere every directory above is enterable (such as %s), or pass --service-user root",
					pf.ServiceUser, dir, strings.TrimPrefix(access.mode.Perm().String(), "-"), i.layout.Binary(), DefaultDir)
			}
		}
	}
	return nil
}
