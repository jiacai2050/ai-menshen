package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	aimenshen "github.com/jiacai2050/ai-menshen/internal"
)

//go:embed configs/example.toml
var defaultConfig []byte

var (
	Version = "dev"
)

func main() {
	cli, err := aimenshen.ParseCLI(os.Args, os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}

	if cli.Version {
		fmt.Printf("ai-menshen %s\n", Version)
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				switch setting.Key {
				case "vcs.revision":
					fmt.Printf("Revision: %s\n", setting.Value)
				case "vcs.time":
					fmt.Printf("Build Time: %s\n", setting.Value)
				}
			}
		}
		return
	}

	if cli.GenConfig {
		os.Stdout.Write(defaultConfig)
		return
	}

	if err := run(cli.ConfigPath); err != nil {
		log.Fatal(err)
	}
}

func run(configPath string) error {
	cfg, err := aimenshen.LoadConfig(configPath)
	if err != nil {
		return err
	}

	storage, err := aimenshen.OpenStorage(cfg.Storage)
	if err != nil {
		return err
	}
	defer func() {
		log.Printf("Storage: closing workers and queue...")
		if err := storage.Close(); err != nil {
			log.Printf("Storage: close failed, err: %v", err)
		} else {
			log.Println("Storage closed successfully")
		}
	}()

	handler, err := aimenshen.NewGateway(cfg, storage)
	if err != nil {
		return err
	}

	log.Printf("ai-menshen API started on %s", cfg.Listen)
	log.Printf("ai-menshen Web UI started on %s", cfg.Web.Listen)
	for i, provider := range cfg.Providers {
		log.Printf("  provider[%d]: %s (weight=%d)", i, provider.BaseURL, provider.Weight)
		if provider.Model != "" {
			log.Printf("  provider[%d] model override: %s", i, provider.Model)
		}
	}
	if len(cfg.Providers) > 1 {
		log.Printf("Weighted load balancing across %d providers", len(cfg.Providers))
	}

	apiServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: http.HandlerFunc(handler.ServeAPIHTTP),
	}
	webServer := &http.Server{
		Addr:    cfg.Web.Listen,
		Handler: http.HandlerFunc(handler.ServeWebHTTP),
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	listenErr := make(chan error, 2)
	go func() {
		listenErr <- fmt.Errorf("api server: %w", apiServer.ListenAndServe())
	}()
	go func() {
		listenErr <- fmt.Errorf("web server: %w", webServer.ListenAndServe())
	}()

	select {
	case <-sig:
		log.Println("Shutting down...")
	case err := <-listenErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server listen: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return webServer.Shutdown(shutdownCtx)
}
