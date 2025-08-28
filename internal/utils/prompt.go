package utils

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// ErrPasswordInterrupted returned when password input is interrupted by signal.
var ErrPasswordInterrupted = errors.New("password input interrupted")

// AskPassword read password with no echo
func AskPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Print(prompt)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	oldState, _ := term.GetState(fd)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	type result struct {
		pwd []byte
		err error
	}
	resCh := make(chan result, 1)

	fmt.Print(prompt)
	go func() { b, e := term.ReadPassword(fd); resCh <- result{b, e} }()

	select {
	case r := <-resCh:
		fmt.Println()
		if r.err != nil {
			return "", r.err
		}
		return string(r.pwd), nil
	case <-sigChan:
		if oldState != nil {
			_ = term.Restore(fd, oldState)
		}
		fmt.Println()
		return "", ErrPasswordInterrupted
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
