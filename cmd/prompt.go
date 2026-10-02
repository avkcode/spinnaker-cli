package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// confirm asks for interactive confirmation of a destructive action.
//
// When stdin is not a terminal the prompt cannot be answered, so the action is
// refused with a message pointing at --force. Failing closed matters here: these
// commands cancel deployments and delete pipeline definitions, and a piped
// invocation that silently proceeded would be the worse default.
func confirm(prompt string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("%s\nrefusing to continue without confirmation because stdin is not a terminal; pass --force to proceed", prompt)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N]: ", prompt)
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}
