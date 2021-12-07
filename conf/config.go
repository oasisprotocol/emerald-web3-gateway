package conf

import (
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
)

// Config contains the CLI configuration.
type Config struct {
	RuntimeID     string `koanf:"runtime_id"`
	NodeAddress   string `koanf:"node_address"`
	EnablePruning bool   `koanf:"enable_pruning"`
	PruningStep   uint64 `koanf:"pruning_step"`

	Log      *LogConfig      `koanf:"log"`
	Database *DatabaseConfig `koanf:"database"`
	Gateway  *GatewayConfig  `koanf:"gateway"`
}

// Validate performs config validation.
func (cfg *Config) Validate() error {
	if cfg.RuntimeID == "" {
		return fmt.Errorf("malformed runtime ID '%s'", cfg.RuntimeID)
	}
	if cfg.NodeAddress == "" {
		return fmt.Errorf("malformed node address '%s'", cfg.NodeAddress)
	}

	if cfg.Log != nil {
		if err := cfg.Log.Validate(); err != nil {
			return err
		}
	}
	if cfg.Database != nil {
		if err := cfg.Database.Validate(); err != nil {
			return fmt.Errorf("database: %w", err)
		}
	}
	if cfg.Gateway != nil {
		if err := cfg.Gateway.Validate(); err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
	}

	return nil
}

// LogConfig contains the logging configuration.
type LogConfig struct {
	Format string `koanf:"format"`
	Level  string `koanf:"level"`
	File   string `koanf:"file"`
}

// Validate validates the logging configuration.
func (cfg *LogConfig) Validate() error {
	var format logging.Format
	if err := format.Set(cfg.Format); err != nil {
		return err
	}
	var level logging.Level
	return level.Set(cfg.Level)
}

// DatabaseConfig is the postgresql database configuration.
type DatabaseConfig struct {
	Host         string `koanf:"host"`
	Port         int    `koanf:"port"`
	DB           string `koanf:"db"`
	User         string `koanf:"user"`
	Password     string `koanf:"password"`
	DialTimeout  int    `koanf:"dial_timeout"`
	ReadTimeout  int    `koanf:"read_timeout"`
	WriteTimeout int    `koanf:"write_timeout"`
}

// Validate validates the database configuration.
func (cfg *DatabaseConfig) Validate() error {
	if cfg.Host == "" {
		return fmt.Errorf("malformed database host: ''")
	}
	// TODO:
	return nil
}

// GatewayConfig is the gateway server configuration.
type GatewayConfig struct {
	// HTTP is the gateway http endpoint config.
	HTTP *GatewayHTTPConfig `koanf:"http"`

	// WS is the gateway websocket endpoint config.
	WS *GatewayWSConfig `koanf:"ws"`

	// ChainID defines the Ethereum network chain id.
	ChainID uint32 `koanf:"chain_id"`
}

// Validate validates the gateway configuration.
func (cfg *GatewayConfig) Validate() error {
	// TODO:
	return nil
}

type GatewayHTTPConfig struct {
	// Host is the host interface on which to start the HTTP RPC server. Defaults to localhost.
	Host string `koanf:"host"`

	// Port is the port number on which to start the HTTP RPC server. Defaults to 8545.
	Port int `koanf:"port"`

	// Cors are the CORS allowed urls.
	Cors []string `koanf:"cors"`

	// VirtualHosts is the list of virtual hostnames which are allowed on incoming requests.
	VirtualHosts []string `koanf:"virtual_hosts"`

	// PathPrefix specifies a path prefix on which http-rpc is to be served. Defaults to '/'.
	PathPrefix string `koanf:"path_prefix"`

	// Timeouts allows for customization of the timeout values used by the HTTP RPC
	// interface.
	Timeouts *HTTPTimeouts `koanf:"timeouts"`
}

type HTTPTimeouts struct {
	Read  *time.Duration `koanf:"read"`
	Write *time.Duration `koanf:"write"`
	Idle  *time.Duration `koanf:"idle"`
}

type GatewayWSConfig struct {
	// Host is the host interface on which to start the HTTP RPC server. Defaults to localhost.
	Host string `koanf:"host"`

	// Port is the port number on which to start the HTTP RPC server. Defaults to 8545.
	Port int `koanf:"port"`

	// PathPrefix specifies a path prefix on which http-rpc is to be served. Defaults to '/'.
	PathPrefix string `koanf:"path_prefix"`

	// Origins is the list of domain to accept websocket requests from.
	Origins []string `koanf:"origins"`

	// Timeouts allows for customization of the timeout values used by the HTTP RPC
	// interface.
	Timeouts *HTTPTimeouts `koanf:"timeouts"`
}

// InitConfig initializes configuration from file.
func InitConfig(f string) (*Config, error) {
	var config Config
	k := koanf.New(".")

	// Load configuration from the yaml config.
	if err := k.Load(file.Provider(f), yaml.Parser()); err != nil {
		return nil, err
	}

	// Load environment variables and merge into the loaded config.
	if err := k.Load(env.Provider("", ".", func(s string) string {
		// `__` is used as a hierarchy delimiter.
		return strings.ReplaceAll(strings.ToLower(s), "__", ".")
	}), nil); err != nil {
		return nil, err
	}

	// Unmarshal into config.
	if err := k.Unmarshal("", &config); err != nil {
		return nil, err
	}

	// Validate config.
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &config, nil
}
