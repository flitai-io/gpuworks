package messages

// Codes of accounts and external identities (ADR 0085): what an identity
// connector, the sign-in routes and the account administration refuse with.
const (
	// ErrorUserDisabled: the account is disabled; sign-in, sign-in tickets,
	// platform tokens and inference API keys of the user are refused.
	ErrorUserDisabled = "USER_DISABLED"
	// ErrorExternalIdentityInvalid: a source name, subject or profile does
	// not have the shape of an external identity.
	ErrorExternalIdentityInvalid = "EXTERNAL_IDENTITY_INVALID"
	// ErrorExternalIdentityTaken: the (source, subject) pair is already
	// linked to an account.
	ErrorExternalIdentityTaken = "EXTERNAL_IDENTITY_TAKEN"
	// ErrorExternalIdentityNotFound: no account is linked to the external
	// identity, or the link named is not the account's.
	ErrorExternalIdentityNotFound = "EXTERNAL_IDENTITY_NOT_FOUND"

	// ErrorIdentityTicketInvalid: a sign-in ticket does not exist, expired,
	// was used, belongs to another identity source, or does not match the
	// browser that started the sign-in.
	ErrorIdentityTicketInvalid = "IDENTITY_TICKET_INVALID"
	// ErrorIdentityProtectedUser: an identity connector tried to disable,
	// enable, rename or sign out an administrator.
	ErrorIdentityProtectedUser = "IDENTITY_PROTECTED_USER"
	// ErrorIdentityNotProvisioned: no account is linked to the external
	// identity and the site creates none (auth.external.provision:
	// existing).
	ErrorIdentityNotProvisioned = "IDENTITY_NOT_PROVISIONED"
	// ErrorIdentityUsernameTaken: another account already has the username
	// ({username}) an identity connector asked for.
	ErrorIdentityUsernameTaken = "IDENTITY_USERNAME_TAKEN"
	// ErrorIdentitySourceConflict: the identity source ({source}) is owned
	// by another extension, or is not one this extension owns.
	ErrorIdentitySourceConflict = "IDENTITY_SOURCE_CONFLICT"
	// ErrorIdentitySourceUnavailable: the identity source ({source}) cannot
	// be used now: no extension of this server offers a sign-in through it,
	// or its feature is locked or hidden.
	ErrorIdentitySourceUnavailable = "IDENTITY_SOURCE_UNAVAILABLE"
)
