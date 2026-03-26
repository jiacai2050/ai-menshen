package main

import (
	"context"
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

	cfg, err := aimenshen.LoadConfig(cli.ConfigPath)
	if err != nil {
		log.Fatal(err)
	}

	storage, err := aimenshen.OpenStorage(cfg.Storage)
	if err != nil {
		log.Fatal(err)
	}
	defer storage.Close()

	handler, err := aimenshen.NewGateway(cfg, storage)
	if err != nil {
		log.Fatal(err)
	}

	provider := cfg.PrimaryProvider()
	log.Printf("ai-menshen started on %s -> %s", cfg.Listen, provider.BaseURL)
	if provider.Model != "" {
		log.Printf("Provider model override enabled: %s", provider.Model)
	}

	server := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	// Server run context
	serverCtx, serverStopCtx := context.WithCancel(context.Background())

	// Listen for syscall signals for process to interrupt/quit
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-sig

		// Shutdown signal with grace period
		shutdownCtx, cancel := context.WithTimeout(serverCtx, 10*time.Second)
		defer cancel()

		go func() {
			<-shutdownCtx.Done()
			if shutdownCtx.Err() == context.DeadlineExceeded {
				log.Fatal("graceful shutdown timed out.. forcing exit.")
			}
		}()

		// Trigger graceful shutdown
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			log.Fatal(err)
		}
		serverStopCtx()
	}()

	// Run the server
	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}

	// Wait for server context to be stopped
	<-serverCtx.Done()
	log.Printf("ai-menshen shutdown complete")
}
