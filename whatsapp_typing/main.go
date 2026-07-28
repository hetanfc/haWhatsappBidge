package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	log.Info("whatsapp typing sensor starting",
		"version", version, "contact", cfg.Name, "publisher", cfg.Publisher,
		"composing_timeout", cfg.ComposingTimeout, "off_delay", cfg.OffDelay)

	pub, err := NewPublisher(cfg, log)
	if err != nil {
		log.Error("publisher setup failed", "err", err)
		os.Exit(1)
	}
	defer pub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tracker := NewTracker(cfg, pub, log)

	// NewWhatsApp opens the database and hands the tracker its saved state, so
	// the first publish already carries the last known values instead of a
	// screenful of "unknown".
	wa, err := NewWhatsApp(ctx, cfg, tracker, log)
	if err != nil {
		log.Error("whatsapp setup failed", "err", err)
		os.Exit(1)
	}
	go tracker.Run(ctx)
	if err := wa.Start(ctx); err != nil {
		log.Error("whatsapp start failed", "err", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	log.Info("shutting down")
	cancel()
	wa.Close()
}
