package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/shyky/memory-proxy/internal/config"
	"github.com/shyky/memory-proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	p, err := proxy.New(cfg)
	if err != nil {
		slog.Error("init proxy", "error", err)
		os.Exit(1)
	}

	if err := p.Start(); err != nil {
		slog.Error("proxy failed", "error", err)
		os.Exit(1)
	}
}