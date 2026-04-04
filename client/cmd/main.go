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
	configPath := flag.String("config", "/etc/eventic/config.yaml", "path to the configuration file")
	flag.StringVar(configPath, "c", "/etc/eventic/config.yaml", "path to the configuration file (shorthand)")
	showVersion := flag.Bool("version", false, "print version and exit")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: eventic-client [options]\n\n")
		fmt.Fprintf(os.Stderr, "Eventic client connects to a relay server and executes\nGitHub webhook-driven actions on local repositories.\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	// Pass build-time version to the client package for auto-update checks.
	client.Version = version

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatal().Err(err).Str("path", *configPath).Msg("failed to read config")
	}

	var cfg client.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatal().Err(err).Msg("failed to parse config")
	}

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

	client.Run(ctx, cfg)
}
