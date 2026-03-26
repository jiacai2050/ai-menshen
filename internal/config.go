package aimenshen

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

type Config struct {
	Listen    string           `toml:"listen"`
	Verbose   bool             `toml:"verbose"`
	Providers []ProviderConfig `toml:"providers"`
	Storage   StorageConfig    `toml:"storage"`
	Cache     CacheConfig      `toml:"cache"`
	Logging   LoggingConfig    `toml:"logging"`
}

type ProviderConfig struct {
	BaseURL string            `toml:"base_url"`
	APIKey  string            `toml:"api_key"`
	Headers map[string]string `toml:"headers"`
	Model   string            `toml:"model"`
}

type StorageConfig struct {
	SQLitePath    string `toml:"sqlite_path"`
	RetentionDays int    `toml:"retention_days"`
}

type CacheConfig struct {
	Enable       bool  `toml:"enable"`
	MaxBodyBytes int64 `toml:"max_body_bytes"`
}

type LoggingConfig struct {
	LogRequestBody  bool `toml:"log_request_body"`
	LogResponseBody bool `toml:"log_response_body"`
}

type CLIOptions struct {
	ConfigPath string
	Version    bool
}

func ParseCLI(args []string, output io.Writer) (CLIOptions, error) {
	options := CLIOptions{ConfigPath: "config.toml"}

	flagSet := flag.NewFlagSet(filepath.Base(args[0]), flag.ContinueOnError)
	flagSet.SetOutput(output)
	flagSet.StringVar(&options.ConfigPath, "config", options.ConfigPath, "path to TOML config file")
	flagSet.BoolVar(&options.Version, "version", false, "print version information")
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Usage: %s [-config path] [-version]\n\n", filepath.Base(args[0]))
		fmt.Fprintln(flagSet.Output(), "Run the OpenAI-compatible gateway with a TOML config file.")
		fmt.Fprintln(flagSet.Output())
		fmt.Fprintln(flagSet.Output(), "Flags:")
		flagSet.PrintDefaults()
		fmt.Fprintln(flagSet.Output())
		fmt.Fprintf(flagSet.Output(), "Examples:\n  %s\n  %s -config ./config.toml\n  %s -version\n", filepath.Base(args[0]), filepath.Base(args[0]), filepath.Base(args[0]))
	}

	if err := flagSet.Parse(args[1:]); err != nil {
		return options, err
	}

	if flagSet.NArg() != 0 {
		return options, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flagSet.Args(), " "))
	}

	return options, nil
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Listen: ":8080",
		Storage: StorageConfig{
			SQLitePath:    "./data/ai-menshen.db",
			RetentionDays: 30,
		},
		Cache: CacheConfig{
			MaxBodyBytes: 1 << 20,
		},
		Logging: LoggingConfig{
			LogRequestBody:  true,
			LogResponseBody: true,
		},
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := toml.Unmarshal(content, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}

	if len(cfg.Providers) == 0 {
		return cfg, fmt.Errorf("config.providers must contain at least one provider")
	}

	for i, provider := range cfg.Providers {
		cfg.Providers[i].APIKey = os.ExpandEnv(provider.APIKey)
		if len(provider.Headers) > 0 {
			expandedHeaders := make(map[string]string, len(provider.Headers))
			for k, v := range provider.Headers {
				expandedHeaders[k] = os.ExpandEnv(v)
			}
			cfg.Providers[i].Headers = expandedHeaders
		}

		if strings.TrimSpace(provider.BaseURL) == "" {
			return cfg, fmt.Errorf("config.providers[%d].base_url is required", i)
		}

		parsed, err := url.Parse(provider.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return cfg, fmt.Errorf("config.providers[%d].base_url is invalid", i)
		}

		cfg.Providers[i].BaseURL = strings.TrimRight(provider.BaseURL, "/")
	}

	return cfg, nil
}

func (c Config) PrimaryProvider() ProviderConfig {
	return c.Providers[0]
}
