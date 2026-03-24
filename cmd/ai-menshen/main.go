package main

import (
	"errors"
	"flag"
	"log"
	"net/http"
	"os"

	aimenshen "ai-menshen/internal"
)

func main() {
	cli, err := aimenshen.ParseCLI(os.Args, os.Stdout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
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
