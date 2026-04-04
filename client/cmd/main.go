package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nitecon/eventic/client"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	configPath := "/etc/eventic/config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatal().Err(err).Str("path", configPath).Msg("failed to read config")
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

	log.Info().Str("relay", cfg.Relay).Str("client_id", cfg.ClientID).Msg("eventic client starting")
	client.Run(ctx, cfg)
}
