package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// At least 8 characters with no composition rules, and a short blocklist of
// guessable passwords instead. Login attempts are throttled per address and
// account, which keeps online guessing slow; the blocklist is not a list of
// breached passwords.
const (
	MinPasswordRunes = 8
	MaxPasswordRunes = 128
	MaxPasswordBytes = 512
)

// ErrPasswordPolicy identifies a rejected new password. Verification of an
// existing password deliberately does not apply the creation policy.
var ErrPasswordPolicy = errors.New("password does not meet the security policy")

// Reasons a PolicyError carries, stable for clients to translate.
const (
	PolicyLength     = "length"     // outside MinPasswordRunes..MaxPasswordRunes or MaxPasswordBytes
	PolicyCharacters = "characters" // not UTF-8, or holds control characters
	PolicyGuessable  = "guessable"  // a common word, repetition or sequence
	PolicyUsername   = "username"   // the account's own name
)

// PolicyError is a rejected new password and why; errors.Is matches
// ErrPasswordPolicy.
type PolicyError struct{ Reason string }

func (e *PolicyError) Error() string {
	switch e.Reason {
	case PolicyLength:
		return fmt.Sprintf("%s: use %d to %d characters, at most %d bytes", ErrPasswordPolicy, MinPasswordRunes, MaxPasswordRunes, MaxPasswordBytes)
	case PolicyCharacters:
		return ErrPasswordPolicy.Error() + ": use valid UTF-8 text without control characters"
	case PolicyUsername:
		return ErrPasswordPolicy.Error() + ": do not use the username as the password"
	}
	return ErrPasswordPolicy.Error() + ": choose an uncommon password or passphrase instead of a common word, repetition or sequence"
}

func (e *PolicyError) Is(target error) bool { return target == ErrPasswordPolicy }

// ValidateAccountPassword is ValidatePassword for a named account: the
// password may also not be the username, give or take case, punctuation and
// surrounding digits ("Admin@2026" for admin).
func ValidateAccountPassword(username, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	name := strings.TrimSpace(username)
	if base := guessableBase(name); (base != "" && guessableBase(password) == base) || (name != "" && strings.EqualFold(strings.TrimSpace(password), name)) {
		return &PolicyError{Reason: PolicyUsername}
	}
	return nil
}

// ValidatePassword is the shared policy for creating or replacing a management
// password. Passphrases and Unicode are supported; character classes are not
// required. It does not trim or otherwise change the password that is hashed.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return &PolicyError{Reason: PolicyCharacters}
	}
	count := utf8.RuneCountInString(password)
	if count < MinPasswordRunes || count > MaxPasswordRunes || len(password) > MaxPasswordBytes {
		return &PolicyError{Reason: PolicyLength}
	}
	for _, r := range password {
		if unicode.IsControl(r) {
			return &PolicyError{Reason: PolicyCharacters}
		}
	}
	pattern := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, password)
	if pattern == "" || commonPassword(guessableBase(password)) || repeatedPassword([]rune(pattern)) || sequentialPassword(pattern) {
		return &PolicyError{Reason: PolicyGuessable}
	}
	return nil
}

// guessableBase is what is left of a password once case, spaces, punctuation,
// symbols and leading or trailing digits are dropped: "P@ssword123!" and
// "password" share the base "password".
func guessableBase(password string) string {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, password)
	return strings.TrimFunc(normalized, unicode.IsNumber)
}

func commonPassword(password string) bool {
	switch password {
	case "password", "passwordpassword", "passwordpasswordpassword", "admin", "adminpassword", "administrator",
		"qwerty", "qwertyuiop", "letmein", "welcome", "welcomewelcome", "changeme", "changeyourpassword",
		"defaultpassword", "thisisapassword", "iloveyou", "iloveyouforever", "gpuworks", "gpuworkspassword",
		"passw0rd", "pssw0rd", "default", "secret", "root", "test", "guest", "user", "abc", "abcd", "qwe", "qwer",
		"asdf", "zxcv", "qaz2wsx", "1qaz2wsx":
		return true
	}
	return false
}

// repeatedPassword rejects an entire password made of the same short unit,
// while leaving repetitions within an otherwise meaningful passphrase alone.
func repeatedPassword(password []rune) bool {
	if len(password) < 2 {
		return true
	}
	for size := 1; size <= 8 && size <= len(password)/2; size++ {
		if len(password)%size != 0 {
			continue
		}
		repeated := true
		for i := size; i < len(password); i++ {
			if password[i] != password[i%size] {
				repeated = false
				break
			}
		}
		if repeated {
			return true
		}
	}
	return false
}

func sequentialPassword(password string) bool {
	for _, sequence := range []string{"0123456789", "9876543210", "abcdefghijklmnopqrstuvwxyz", "zyxwvutsrqponmlkjihgfedcba", "1234567890qwertyuiopasdfghjklzxcvbnm", "mnbvcxzlkjhgfdsapoiuytrewq0987654321", "qwertyuiop", "poiuytrewq", "asdfghjkl", "lkjhgfdsa", "zxcvbnm", "mnbvcxz"} {
		if strings.Contains(strings.Repeat(sequence, len(password)/len(sequence)+2), password) {
			return true
		}
	}
	return false
}

// NewPassword supplies a fresh management password with 192 bits of entropy.
// Entropy failure is returned to the caller before an account or file is saved.
func NewPassword() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate admin password: %w", err)
	}
	password := hex.EncodeToString(random[:])
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	return password, nil
}
