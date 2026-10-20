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
	"slices"
	"strings"
	"syscall"
	"time"

	menshen "github.com/jiacai2050/menshen/internal"
)

//go:embed configs/example.toml
var defaultConfig []byte

var (
	Version = "dev"
)

func main() {
	cli, err := menshen.ParseCLI(os.Args, os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}

	if cli.Version {
		fmt.Printf("menshen %s\n", Version)
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
	cfg, err := menshen.LoadConfig(configPath)
	if err != nil {
		return err
	}

	storage, err := menshen.OpenStorage(cfg.Storage)
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

	handler, err := menshen.NewGateway(cfg, storage)
	if err != nil {
		return err
	}

	log.Printf("menshen API started on %s", cfg.Listen)
	log.Printf("menshen Web UI started on %s", cfg.Web.Listen)
	for _, provider := range sortedProviders(cfg.Providers) {
		log.Printf("  provider[%s]: %s (weight=%d, capabilities=%v)", provider.Name, provider.BaseURL, provider.Weight, provider.Capabilities)
		if provider.Model != "" {
			log.Printf("  provider[%s] model override: %s", provider.Name, provider.Model)
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

// sortedProviders returns providers ordered by name so startup logs are stable.
func sortedProviders(providers map[string]menshen.ProviderConfig) []menshen.ProviderConfig {
	sorted := make([]menshen.ProviderConfig, 0, len(providers))
	for _, provider := range providers {
		sorted = append(sorted, provider)
	}
	slices.SortFunc(sorted, func(a, b menshen.ProviderConfig) int {
		return strings.Compare(a.Name, b.Name)
	})
	return sorted
}
