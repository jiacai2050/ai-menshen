package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"

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

	storage, err := aimenshen.OpenStorage(cfg.Storage.SQLitePath)
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

	log.Fatal(http.ListenAndServe(cfg.Listen, handler))
}
