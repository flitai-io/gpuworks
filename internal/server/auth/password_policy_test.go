package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestManagementPasswordPolicy(t *testing.T) {
	for _, password := range []string{
		"", "123", "1234567", "password", "admin123", "Admin@123", "P@ssw0rd", "qwerty123", "abc12345",
		"abcd1234", "1qaz2wsx", "12345678", "87654321", "11111111", "我的密码很安全", "                ",
		"password123456789", "1234567890123456789", "PASSWORD!PASSWORD!",
		"admin-1234567890123", "abcdefghijklmnop", "abcdabcdabcdabcd", "水水水水水水水水水水水水水水水",
		"qwertyuiopqwertyuiop", "uncommon phrase\nwith newline", string([]byte{0xff, 0xfe}),
		"qwertyuiopasdfghjkl", "1234567890qwertyuiop",
		"              a", "a b c d e f g h", "q q q q q q q q",
		strings.Repeat("long ", 30),
	} {
		if err := ValidatePassword(password); !errors.Is(err, ErrPasswordPolicy) {
			t.Errorf("unsafe password was not rejected with policy error: %v", err)
		}
	}
	for _, password := range []string{
		"fourteenchars!", "gpu2026x", "a1b2c3d4", "我的GPU集群密码", "Tiger#Lake9",
		"seven teal otters cross the meadow", "a very long lowercase passphrase",
		"gpuworks-test-password-2026", "遥远的星河之上有一座安静的图书馆",
		" uncommon phrase with surrounding spaces ", "1234567890abcdef", "1234567890abcdef1234567890abcdef",
		strings.Repeat("😀", 63) + "天", // 64 Unicode characters, not 64 bytes.
	} {
		if err := ValidatePassword(password); err != nil {
			t.Errorf("valid password rejected: %v", err)
		}
	}
}

func TestManagementPasswordPolicyBounds(t *testing.T) {
	for _, length := range []int{MinPasswordRunes - 1, MinPasswordRunes, MaxPasswordRunes, MaxPasswordRunes + 1} {
		password := strings.Repeat("a", length-1) + "z"
		err := ValidatePassword(password)
		if want := length >= MinPasswordRunes && length <= MaxPasswordRunes; (err == nil) != want {
			t.Errorf("length %d: valid=%v want=%v", length, err == nil, want)
		}
	}
	if err := ValidatePassword(strings.Repeat("😀", MaxPasswordRunes-1) + "天"); err != nil {
		t.Fatalf("valid multi-byte boundary rejected: %v", err)
	}
}

func TestGeneratedManagementPasswordsMeetPolicy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		password, err := NewPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(password) != 48 || seen[password] {
			t.Fatal("generated password has the wrong length or repeats a prior sample")
		}
		seen[password] = true
		if err := ValidatePassword(password); err != nil {
			t.Fatalf("generated password rejected: %v", err)
		}
	}
}

func TestManagementPasswordPolicyReasons(t *testing.T) {
	for password, want := range map[string]string{
		"short":                    PolicyLength,
		strings.Repeat("x", 129):   PolicyLength,
		"tab\tseparated":           PolicyCharacters,
		string([]byte{0xff, 0xfe}): PolicyCharacters,
		"Password123":              PolicyGuessable,
		"Admin@2026":               PolicyGuessable,
	} {
		var policy *PolicyError
		if err := ValidatePassword(password); !errors.As(err, &policy) || policy.Reason != want || !errors.Is(err, ErrPasswordPolicy) {
			t.Errorf("%q: got %v, want reason %s", password, err, want)
		}
	}
}

func TestAccountPasswordRejectsTheUsername(t *testing.T) {
	for _, tc := range []struct{ user, password string }{
		{"zhangsan", "zhangsan"}, {"zhangsan", "ZhangSan2026"}, {"zhangsan", "zhang.san!"},
		{"ops-team", "Ops Team 01"}, {"wang.wei", "wangwei@123"},
	} {
		var policy *PolicyError
		if err := ValidateAccountPassword(tc.user, tc.password); !errors.As(err, &policy) || policy.Reason != PolicyUsername {
			t.Errorf("%s/%q: got %v, want the username reason", tc.user, tc.password, err)
		}
	}
	for _, tc := range []struct{ user, password string }{
		{"zhangsan", "zhangsan likes tea"}, {"admin", "Tiger#Lake9"}, {"2026", "20261110"}, {"", "Tiger#Lake9"},
	} {
		if err := ValidateAccountPassword(tc.user, tc.password); err != nil {
			t.Errorf("%s/%q rejected: %v", tc.user, tc.password, err)
		}
	}
	if err := ValidateAccountPassword("zhangsan", "short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("account policy skipped the shared policy: %v", err)
	}
}
