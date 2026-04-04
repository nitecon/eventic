package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/nitecon/eventic/server"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	cfg := server.Config{
		WebhookSecret: getEnv("EVENTIC_WEBHOOK_SECRET", ""),
		ListenAddr:    getEnv("EVENTIC_LISTEN_ADDR", ":8080"),
		ClientTokens:  parseTokens(getEnv("EVENTIC_CLIENT_TOKENS", "")),
	}

	if cfg.WebhookSecret == "" {
		log.Fatal().Msg("EVENTIC_WEBHOOK_SECRET is required")
	}
	if len(cfg.ClientTokens) == 0 {
		log.Fatal().Msg("EVENTIC_CLIENT_TOKENS is required")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := server.Start(cfg); err != nil {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	log.Info().Msg("eventic server running")
	<-sigCh
	log.Info().Msg("shutting down")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseTokens(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tokens[t] = true
		}
	}
	return tokens
}
