// Package config loads YAML configuration with environment overrides.
// Secrets never live in the committed config; use .env / environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"crossvenue/internal/domain"
	"crossvenue/pkg/decimal"
)

// Mode selects the runtime pipeline source.
type Mode string

const (
	ModeLiveSim   Mode = "live-market-sim"
	ModeReplay    Mode = "replay"
	ModeSynthetic Mode = "synthetic"
)

// Config is the root configuration.
type Config struct {
	Mode    Mode     `yaml:"mode"`
	Symbols []string `yaml:"symbols"`

	Venues map[string]VenueConfig `yaml:"venues"`

	Risk struct {
		MaxTradeNotionalUSD  string `yaml:"max_trade_notional_usd"`
		MaxSymbolExposureUSD string `yaml:"max_symbol_exposure_usd"`
		MaxVenueExposureUSD  string `yaml:"max_venue_exposure_usd"`
		MaxTotalExposureUSD  string `yaml:"max_total_exposure_usd"`
		MaxQuoteAgeMs        int    `yaml:"max_quote_age_ms"`
		MinEdgeBps           int64  `yaml:"min_edge_bps"`
		MinProfitUSD         string `yaml:"min_profit_usd"`
		MaxOutstandingOrders int    `yaml:"max_outstanding_orders"`
		MaxConsecutiveFails  int    `yaml:"max_consecutive_failures"`
		DailyLossLimitUSD    string `yaml:"daily_loss_limit_usd"`
		KillSwitch           bool   `yaml:"kill_switch"`
	} `yaml:"risk"`

	Execution struct {
		Mode        string `yaml:"mode"`         // simulation (only supported default)
		LatencyMode string `yaml:"latency_mode"` // instant|fixed|sampled
		Policy      string `yaml:"policy"`       // parallel|buy-first|sell-first
		TradeQty    string `yaml:"trade_quantity"`
		SlippageBps int64  `yaml:"slippage_bps"`
		Seed        int64  `yaml:"seed"`
	} `yaml:"execution"`

	Storage struct {
		PostgresURL string `yaml:"postgres_url"`
	} `yaml:"storage"`

	Recording struct {
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"`
	} `yaml:"recording"`

	Replay struct {
		File  string `yaml:"file"`
		Speed string `yaml:"speed"` // "1x", "10x", "max"
	} `yaml:"replay"`

	Synthetic struct {
		Seed         int64 `yaml:"seed"`
		EventsPerSec int   `yaml:"events_per_second"`
		GapEvery     int   `yaml:"gap_every"`
	} `yaml:"synthetic"`

	API struct {
		Listen string `yaml:"listen"`
	} `yaml:"api"`
}

// VenueConfig per venue.
type VenueConfig struct {
	Enabled      bool   `yaml:"enabled"`
	TakerBps     int64  `yaml:"taker_bps"`
	MakerBps     int64  `yaml:"maker_bps"`
	OutboundUS   int64  `yaml:"outbound_us"`
	InitialBase  string `yaml:"initial_base"`  // e.g. "2" BTC
	InitialQuote string `yaml:"initial_quote"` // e.g. "100000" USDT
}

// Load reads a YAML file, then applies environment overrides.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	// Environment overrides: CROSSVENUE_POSTGRES_URL, CROSSVENUE_API_LISTEN,
	// CROSSVENUE_MODE.
	if v := os.Getenv("CROSSVENUE_POSTGRES_URL"); v != "" {
		c.Storage.PostgresURL = v
	}
	if v := os.Getenv("CROSSVENUE_API_LISTEN"); v != "" {
		c.API.Listen = v
	}
	if v := os.Getenv("CROSSVENUE_MODE"); v != "" {
		c.Mode = Mode(v)
	}
	// Live execution requires an explicit, separate opt-in and is never
	// enabled by config alone.
	if strings.EqualFold(os.Getenv("ENABLE_LIVE_EXECUTION"), "true") {
		return nil, fmt.Errorf("config: live execution is not implemented; unset ENABLE_LIVE_EXECUTION")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Default returns the safe simulation defaults.
func Default() *Config {
	c := &Config{
		Mode:    ModeSynthetic,
		Symbols: []string{"BTC-USDT"},
		Venues: map[string]VenueConfig{
			"binance": {Enabled: true, TakerBps: 10, MakerBps: 10, OutboundUS: 5000, InitialBase: "2", InitialQuote: "100000"},
			"okx":     {Enabled: true, TakerBps: 10, MakerBps: 8, OutboundUS: 7000, InitialBase: "3", InitialQuote: "200000"},
			"bybit":   {Enabled: true, TakerBps: 10, MakerBps: 10, OutboundUS: 6000, InitialBase: "1", InitialQuote: "150000"},
		},
	}
	c.Risk.MaxTradeNotionalUSD = "10000"
	c.Risk.MaxSymbolExposureUSD = "25000"
	c.Risk.MaxVenueExposureUSD = "50000"
	c.Risk.MaxTotalExposureUSD = "100000"
	c.Risk.MaxQuoteAgeMs = 750
	c.Risk.MinEdgeBps = 3
	c.Risk.MinProfitUSD = "1"
	c.Risk.MaxOutstandingOrders = 64
	c.Risk.MaxConsecutiveFails = 10
	c.Risk.DailyLossLimitUSD = "1000"
	c.Execution.Mode = "simulation"
	c.Execution.LatencyMode = "fixed"
	c.Execution.Policy = "parallel"
	c.Execution.TradeQty = "0.01"
	c.Execution.SlippageBps = 1
	c.Synthetic.Seed = 42
	c.Synthetic.EventsPerSec = 200
	c.API.Listen = "127.0.0.1:8471"
	return c
}

// Validate checks consistency.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLiveSim, ModeReplay, ModeSynthetic:
	default:
		return fmt.Errorf("config: unknown mode %q", c.Mode)
	}
	if len(c.Symbols) == 0 {
		return fmt.Errorf("config: no symbols configured")
	}
	any := false
	for _, v := range c.Venues {
		if v.Enabled {
			any = true
		}
	}
	if !any {
		return fmt.Errorf("config: no venues enabled")
	}
	for k, v := range c.Venues {
		switch domain.Venue(k) {
		case domain.VenueBinance, domain.VenueOKX, domain.VenueBybit, domain.VenueSynthetic:
		default:
			return fmt.Errorf("config: unknown venue %q", k)
		}
		if v.InitialBase != "" {
			if _, err := decimal.Parse(v.InitialBase); err != nil {
				return fmt.Errorf("config: venue %s initial_base: %w", k, err)
			}
		}
		if v.InitialQuote != "" {
			if _, err := decimal.Parse(v.InitialQuote); err != nil {
				return fmt.Errorf("config: venue %s initial_quote: %w", k, err)
			}
		}
	}
	if _, err := decimal.Parse(c.Execution.TradeQty); err != nil {
		return fmt.Errorf("config: execution.trade_quantity: %w", err)
	}
	return nil
}

// MaxQuoteAge returns the staleness threshold.
func (c *Config) MaxQuoteAge() time.Duration {
	return time.Duration(c.Risk.MaxQuoteAgeMs) * time.Millisecond
}

// EnabledVenues lists enabled venues in stable order.
func (c *Config) EnabledVenues() []domain.Venue {
	order := []domain.Venue{domain.VenueBinance, domain.VenueOKX, domain.VenueBybit, domain.VenueSynthetic}
	var out []domain.Venue
	for _, v := range order {
		if vc, ok := c.Venues[string(v)]; ok && vc.Enabled {
			out = append(out, v)
		}
	}
	return out
}
