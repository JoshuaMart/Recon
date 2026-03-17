package cmd

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

func HashPassword() {
	fmt.Fprint(os.Stderr, "Password: ")
	password, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading password: %v\n", err)
		os.Exit(1)
	}

	if len(password) == 0 {
		fmt.Fprintln(os.Stderr, "Error: password cannot be empty")
		os.Exit(1)
	}

	hash, err := bcrypt.GenerateFromPassword(password, 12)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error hashing password: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(hash))
}
