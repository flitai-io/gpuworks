package app

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The store's account errors become the codes a connector reads; the taken
// username is named, and an unknown error passes through.
func TestAccountStoreErrorsBecomeProductCodes(t *testing.T) {
	for err, want := range map[error]string{
		store.ErrNotFound:         messages.ErrorExternalIdentityNotFound,
		store.ErrProtectedAccount: messages.ErrorIdentityProtectedUser,
		store.ErrUsernameTaken:    messages.ErrorIdentityUsernameTaken,
		store.ErrIdentityTaken:    messages.ErrorExternalIdentityTaken,
		store.ErrInvalidIdentity:  messages.ErrorExternalIdentityInvalid,
	} {
		var pe *messages.ProductError
		if got := accountError(err, "alice"); !errors.As(got, &pe) || pe.Code() != want {
			t.Fatalf("%v = %v, want %s", err, got, want)
		}
	}
	var pe *messages.ProductError
	if !errors.As(accountError(store.ErrUsernameTaken, "alice"), &pe) || pe.Params()["username"] != "alice" {
		t.Fatalf("a taken username names no username: %v", pe)
	}
	other := errors.New("database is locked")
	if got := accountError(other, ""); got != other {
		t.Fatalf("an unknown error became %v", got)
	}
	if accountError(nil, "") != nil {
		t.Fatal("nil became an error")
	}
}
