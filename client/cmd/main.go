package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nitecon/eventic/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "approve":
		approveCmd(os.Args[2:])
	case "revoke":
		revokeCmd(os.Args[2:])
	case "list-approvals":
		listApprovalsCmd(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		// Default to "run" if the first arg is a flag or unknown
		if len(os.Args[1]) > 0 && os.Args[1][0] == '-' {
			runCmd(os.Args[1:])
		} else {
			printUsage()
			os.Exit(1)
		}
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: eventic-client <command> [options]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  run              Connect to relay and process events (default)\n")
	fmt.Fprintf(os.Stderr, "  approve          Approve a repository or sender\n")
	fmt.Fprintf(os.Stderr, "  revoke           Revoke approval for a repository or sender\n")
	fmt.Fprintf(os.Stderr, "  list-approvals   List all approved repositories and senders\n")
	fmt.Fprintf(os.Stderr, "  version          Print version\n")
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "/etc/eventic/config.yaml", "path to the configuration file")
	fs.StringVar(configPath, "c", "/etc/eventic/config.yaml", "path to the configuration file (shorthand)")
	fs.Parse(args)

	client.Version = version

	cfg := loadConfig(*configPath)
	setupLogLevel(cfg.LogLevel)

	if cfg.Relay == "" || cfg.Token == "" || cfg.ClientID == "" {
		log.Fatal().Msg("relay, token, and client_id are required in config")
	}
	if cfg.ReposDir == "" {
		cfg.ReposDir = "/opt/eventic/repos"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Info().Msg("shutting down")
		cancel()
	}()

	log.Info().Str("relay", cfg.Relay).Str("client_id", cfg.ClientID).Str("version", version).Msg("eventic client starting")

	if cfg.AutoUpdate {
		go client.StartAutoUpdater(ctx)
	}

	if cfg.AutoCheck == nil || *cfg.AutoCheck {
		go client.StartRepoChecker(ctx, cfg)
	}

	client.Run(ctx, cfg)
}

func approveCmd(args []string) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	configPath := fs.String("config", "/etc/eventic/config.yaml", "path to config (to find approvals_path)")
	repo := fs.String("repo", "", "Repository to approve (e.g. org/repo)")
	sender := fs.String("sender", "", "GitHub sender to approve (username)")
	fs.Parse(args)

	if *repo == "" && *sender == "" {
		fmt.Fprintf(os.Stderr, "Error: either --repo or --sender must be specified\n")
		fs.Usage()
		os.Exit(1)
	}

	store := loadApprovalStore(*configPath)

	if *repo != "" {
		if err := store.ApproveRepo(*repo); err != nil {
			log.Fatal().Err(err).Msg("failed to approve repo")
		}
		fmt.Printf("Approved repository: %s\n", *repo)
	}

	if *sender != "" {
		if err := store.ApproveSender(*sender); err != nil {
			log.Fatal().Err(err).Msg("failed to approve sender")
		}
		fmt.Printf("Approved sender: %s\n", *sender)
	}
}

func revokeCmd(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	configPath := fs.String("config", "/etc/eventic/config.yaml", "path to config (to find approvals_path)")
	repo := fs.String("repo", "", "Repository to revoke (e.g. org/repo)")
	sender := fs.String("sender", "", "GitHub sender to revoke (username)")
	fs.Parse(args)

	if *repo == "" && *sender == "" {
		fmt.Fprintf(os.Stderr, "Error: either --repo or --sender must be specified\n")
		fs.Usage()
		os.Exit(1)
	}

	store := loadApprovalStore(*configPath)

	if *repo != "" {
		if err := store.RevokeRepo(*repo); err != nil {
			log.Fatal().Err(err).Msg("failed to revoke repo")
		}
		fmt.Printf("Revoked repository: %s\n", *repo)
	}

	if *sender != "" {
		if err := store.RevokeSender(*sender); err != nil {
			log.Fatal().Err(err).Msg("failed to revoke sender")
		}
		fmt.Printf("Revoked sender: %s\n", *sender)
	}
}

func listApprovalsCmd(args []string) {
	fs := flag.NewFlagSet("list-approvals", flag.ExitOnError)
	configPath := fs.String("config", "/etc/eventic/config.yaml", "path to config (to find approvals_path)")
	fs.Parse(args)

	store := loadApprovalStore(*configPath)
	fmt.Print(store.List())
}

func loadApprovalStore(configPath string) *client.ApprovalStore {
	cfg := loadConfig(configPath)
	approvalsPath := cfg.ApprovalsPath
	if approvalsPath == "" {
		approvalsPath = "/etc/eventic/approvals.json"
	}
	return client.NewApprovalStore(approvalsPath)
}

func loadConfig(path string) client.Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal().Err(err).Str("path", path).Msg("failed to read config")
	}

	var cfg client.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatal().Err(err).Msg("failed to parse config")
	}
	return cfg
}

func setupLogLevel(lvl string) {
	level := zerolog.InfoLevel
	if lvl != "" {
		if parsed, err := zerolog.ParseLevel(lvl); err == nil {
			level = parsed
		} else {
			log.Warn().Str("log-level", lvl).Msg("invalid log level, defaulting to info")
		}
	}
	zerolog.SetGlobalLevel(level)
}
