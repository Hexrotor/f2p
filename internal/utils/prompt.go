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
func AskPassword(prompt string) (string, error) {
	termState, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return "", fmt.Errorf("failed to get terminal state: %v", err)
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go func() {
		<-sigChan
		fmt.Println("\n\nPassword input cancelled")
		term.Restore(int(os.Stdin.Fd()), termState)
		os.Exit(1)
	}()

	fmt.Print(prompt)
	fd := int(os.Stdin.Fd())
	bytePwd, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", err
	}
	return string(bytePwd), nil
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
