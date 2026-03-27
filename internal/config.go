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
	Listen     string           `toml:"listen"`
	Verbose    bool             `toml:"verbose"`
	Auth       AuthConfig       `toml:"auth"`
	Providers  []ProviderConfig `toml:"providers"`
	HTTPClient HTTPClientConfig `toml:"http_client"`
	Storage    StorageConfig    `toml:"storage"`
	Cache      CacheConfig      `toml:"cache"`
	Logging    LoggingConfig    `toml:"logging"`
}

type AuthConfig struct {
	Enable   bool   `toml:"enable"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Token    string `toml:"token"`
}

type ProviderConfig struct {
	BaseURL string            `toml:"base_url"`
	APIKey  string            `toml:"api_key"`
	Headers map[string]string `toml:"headers"`
	Model   string            `toml:"model"`
}

type HTTPClientConfig struct {
	Timeout int `toml:"timeout"`
}

type SQLiteConfig struct {
	Path string `toml:"path"`
}

type StorageConfig struct {
	RetentionDays int          `toml:"retention_days"`
	SQLite        SQLiteConfig `toml:"sqlite"`
}

type CacheConfig struct {
	Enable       bool  `toml:"enable"`
	MaxBodyBytes int64 `toml:"max_body_bytes"`
	MaxAge       int64 `toml:"max_age"`
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
		HTTPClient: HTTPClientConfig{
			Timeout: 300,
		},
		Storage: StorageConfig{
			SQLite: SQLiteConfig{
				Path: "./data/ai-menshen.db",
			},
			RetentionDays: 90,
		},
		Cache: CacheConfig{
			Enable:       true,
			MaxBodyBytes: 5 << 20,
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

	if cfg.Auth.Enable {
		cfg.Auth.Password = os.ExpandEnv(cfg.Auth.Password)
		cfg.Auth.Token = os.ExpandEnv(cfg.Auth.Token)

		if strings.TrimSpace(cfg.Auth.User) == "" {
			return cfg, fmt.Errorf("config.auth.user is required when auth is enabled")
		}
		if strings.TrimSpace(cfg.Auth.Password) == "" {
			return cfg, fmt.Errorf("config.auth.password is required when auth is enabled")
		}
		if strings.TrimSpace(cfg.Auth.Token) == "" {
			return cfg, fmt.Errorf("config.auth.token is required when auth is enabled")
		}
	}

	if cfg.HTTPClient.Timeout < 0 {
		return cfg, fmt.Errorf("config.http_client.timeout must not be negative")
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

	expandedSQLitePath := strings.TrimSpace(os.ExpandEnv(cfg.Storage.SQLite.Path))
	if expandedSQLitePath == "" {
		return cfg, fmt.Errorf("config.storage.sqlite.path is required")
	}
	cfg.Storage.SQLite.Path = expandedSQLitePath

	return cfg, nil
}

func (c Config) PrimaryProvider() ProviderConfig {
	return c.Providers[0]
}
