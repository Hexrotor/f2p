package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Hexrotor/f2p/internal/client"
	"github.com/Hexrotor/f2p/internal/config"
	"github.com/Hexrotor/f2p/internal/logger"
	"github.com/Hexrotor/f2p/internal/server"
	"github.com/Hexrotor/f2p/internal/utils"
)

const DefaultConfigFile = "config.toml"

func main() {
	configPath := DefaultConfigFile
	var changePassword, regenerateIdentity, showHelp, generateConfig bool
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch arg {
		case "--config", "-c":
			if i+1 < len(os.Args) {
				configPath = os.Args[i+1]
				i++
			}
		case "--change-pwd":
			changePassword = true
		case "--regenerate-id", "-r":
			regenerateIdentity = true
		case "--generate", "-g":
			generateConfig = true
		case "--help", "-h":
			showHelp = true
		default:
			if !strings.HasPrefix(arg, "-") && configPath == DefaultConfigFile {
				configPath = arg
			}
		}
	}

	if showHelp {
		printHelp()
		return
	}

	if generateConfig || (!changePassword && !regenerateIdentity && !fileExists(configPath)) {
		fmt.Println("Generating new configuration...")

		if err := config.GenerateInteractiveConfig(configPath); err != nil {
			fmt.Printf("Failed to generate config: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Configuration generated: %s\n\n", configPath)
		fmt.Println("Setup complete! Run the program again to start.")
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Printf("Failed to load config file '%s': %v\n", configPath, err)
		os.Exit(1)
	}

	// Initialize logger with the configured log level
	logger.InitLogger(cfg.Common.LogLevel)

	if changePassword {
		if err := handleChangePassword(cfg, configPath); err != nil {
			fmt.Printf("Failed to change password: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if regenerateIdentity {
		if err := handleRegenerateIdentity(cfg, configPath); err != nil {
			fmt.Printf("Failed to regenerate identity: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := cfg.ValidateConfig(); err != nil {
		fmt.Printf("Configuration validation failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loaded configuration from: %s\n", configPath)

	if cfg.IsServer {
		if err := runServer(cfg); err != nil {
			fmt.Printf("Server error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := runClient(cfg); err != nil {
			fmt.Printf("Client error: %v\n", err)
			os.Exit(1)
		}
	}
}

func runServer(cfg *config.Config) error {
	configPath := getConfigPathFromArgs()
	if err := config.Save(cfg, configPath); err != nil {
		fmt.Printf("Warning: Failed to save updated config: %v\n", err)
	}

	s := server.NewServer(cfg)
	if err := s.Start(); err != nil {
		return err
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		_ = s.Stop()
	}()

	s.Wait()
	return nil
}

func runClient(cfg *config.Config) error {
	c := client.NewClient(cfg)
	if err := c.Start(); err != nil {
		return err
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		_ = c.Stop()
	}()

	c.Wait()
	return nil
}

func getConfigPathFromArgs() string {
	configPath := DefaultConfigFile
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--config" || arg == "-c" {
			if i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		} else if !strings.HasPrefix(arg, "-") && configPath == DefaultConfigFile {
			return arg
		}
	}
	return configPath
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

func printHelp() {
	fmt.Print(`F2P - Service forwarding over p2p connection, based on libp2p

Usage:
  f2p [options] [config_file]

Options:
  --config, -c <file>      Specify config file (default: config.toml)
  --generate, -g           Generate new configuration interactively (Will overwrite existing config)
  --change-pwd             Change server password
  --regenerate-id, -r      Regenerate server identity (Will change server's peerID but keep other configs)
  --help, -h               Show this help message

Examples:
  f2p                      # Run with config.toml
  f2p server.toml          # Run with server.toml
`)
}

func handleChangePassword(cfg *config.Config, configPath string) error {
	if !cfg.IsServer {
		return fmt.Errorf("password can only be changed for server configurations")
	}

	fmt.Println("Changing server password...")

	if utils.AskYesNo("Do you want to set/update a password? (n to remove password): ") {
		password := utils.AskPassword("Enter server password: ")
		confirmPassword := utils.AskPassword("Confirm server password: ")

		if password != confirmPassword {
			return fmt.Errorf("passwords do not match")
		}

		cfg.Server.PasswordHash = config.HashPassword(password)
		fmt.Println("Password updated successfully")
	} else {
		cfg.Server.PasswordHash = ""
		fmt.Println("Password authentication disabled")
	}

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("failed to save config: %v", err)
	}

	fmt.Printf("Configuration saved to: %s\n", configPath)
	return nil
}

func handleRegenerateIdentity(cfg *config.Config, configPath string) error {
	if !cfg.IsServer {
		return fmt.Errorf("identity can only be regenerated for server configurations")
	}

	fmt.Print(`WARNING: Regenerating server identity
   This will change the server's shareString and make it incompatible
   with existing clients. All clients will need the new shareString.
`)

	if !utils.AskYesNo("Are you sure you want to regenerate the server identity? (y/n): ") {
		fmt.Println("Operation cancelled.")
		return nil
	}

	fmt.Println("Regenerating server identity...")

	privKeyB64, peerID, err := config.GeneratePeerIdentity()
	if err != nil {
		return fmt.Errorf("failed to generate new identity: %v", err)
	}

	cfg.Identity.PrivateKey = privKeyB64
	cfg.Identity.PeerID = peerID

	if err := config.Save(cfg, configPath); err != nil {
		return fmt.Errorf("failed to save config: %v", err)
	}

	fmt.Println("Server identity regenerated successfully")
	fmt.Printf("New Peer ID: %s\n", peerID)
	fmt.Println("   Share this new Peer ID with clients to connect.")
	fmt.Printf("Configuration saved to: %s\n", configPath)

	return nil
}
