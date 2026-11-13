package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"
)

// errPasswordMismatch is two different answers to the reset-password prompt.
var errPasswordMismatch = errors.New("the two passwords differ; nothing was changed")

// promptNewPassword asks for the new password twice without echo, so it stays
// out of shell history and the process list. It is a variable for tests.
var promptNewPassword = func() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("-password is required when stdin is not a terminal")
	}
	read := func(question string) (string, error) {
		fmt.Fprint(os.Stderr, question)
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(raw), err
	}
	first, err := read("New password: ")
	if err != nil {
		return "", err
	}
	second, err := read("Repeat the new password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errPasswordMismatch
	}
	return first, nil
}
