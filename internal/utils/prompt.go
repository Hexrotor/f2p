package utils

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// AskPassword reads a password from terminal without echo. It gracefully
// handles non-TTY environments and Ctrl+C interruptions on Windows/Linux.
func AskPassword(prompt string) string {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	fd := int(syscall.Stdin)

	oldState, err := term.GetState(fd)
	if err != nil {
		// Fallback when terminal state cannot be obtained (e.g., non-TTY)
		fmt.Print(prompt)
		var password string
		fmt.Scanln(&password)
		return password
	}

	done := make(chan bool, 1)
	var password string
	var readErr error

	go func() {
		defer func() {
			term.Restore(fd, oldState)
			done <- true
		}()

		fmt.Print(prompt)
		var passwordBytes []byte
		passwordBytes, readErr = term.ReadPassword(fd)
		fmt.Println()

		if readErr == nil {
			password = string(passwordBytes)
		}
	}()

	select {
	case <-done:
		if readErr != nil {
			fmt.Fprintf(os.Stderr, "Error reading password: %v\n", readErr)
			return ""
		}
		return password
	case <-sigChan:
		fmt.Println("\n\nOperation cancelled by user")
		term.Restore(fd, oldState)
		os.Exit(1)
		return ""
	}
}

// AskYesNoWithCancel behaves like AskYesNo but terminates the program on Ctrl+C.
func AskYesNo(prompt string) bool {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Reset(os.Interrupt, syscall.SIGTERM)

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print(prompt)

		type result struct {
			response string
			err      error
		}

		responseChan := make(chan result, 1)
		done := make(chan bool, 1)

		go func() {
			defer func() { done <- true }()
			response, err := reader.ReadString('\n')
			responseChan <- result{
				response: strings.ToLower(strings.TrimSpace(response)),
				err:      err,
			}
		}()

		select {
		case res := <-responseChan:
			<-done
			if res.err != nil {
				fmt.Println("\nConfiguration cancelled by user")
				os.Exit(1)
			}
			switch res.response {
			case "y", "yes":
				return true
			case "n", "no":
				return false
			}
			fmt.Println("Please enter 'y' or 'n'")
		case <-sigChan:
			fmt.Println("\n\nConfiguration cancelled by user")
			<-done
			os.Exit(1)
			return false
		}
	}
}
