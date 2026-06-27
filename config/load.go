package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"checkdiff/schedule"
	"checkdiff/source"
)

// Load reads and parses a TOML config file, applies defaults,
// validates each source via its registered Fetcher, and
// returns the loaded Config. Missing file is propagated as
// fs.ErrNotExist (wrapped), which the caller in main checks
// to decide whether to run the first-run generator.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Ntfy.Topic == "" {
		return nil, fmt.Errorf("config: ntfy.topic is required")
	}
	if c.Ntfy.Server == "" {
		c.Ntfy.Server = "https://ntfy.sh"
	}
	if c.Check.Interval == "" {
		c.Check.Interval = "1h"
	}
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8080"
	}
	if _, err := schedule.Parse(c.Check.Interval); err != nil {
		return nil, fmt.Errorf("config: check.check_interval: %w", err)
	}
	seen := make(map[string]bool, len(c.Sources))
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.ID == "" {
			return nil, fmt.Errorf("config: source[%d]: id is required", i)
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("config: duplicate source id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Name == "" {
			s.Name = s.ID
		}
		if s.CheckInterval != "" {
			// Per-source interval accepts either a Go duration
			// (>= 1 minute) or a 5-field cron expression. The
			// format is auto-detected; the schedule package is
			// the only place the choice is made.
			if _, err := schedule.Parse(s.CheckInterval); err != nil {
				return nil, fmt.Errorf("config: source %q: check_interval: %w", s.ID, err)
			}
		}
		if err := source.Validate(s); err != nil {
			return nil, fmt.Errorf("config: source %q: %w", s.ID, err)
		}
	}
	return &c, nil
}

// Marshal encodes cfg to TOML. The web UI rewrites the config
// on every change, so the format produced here is what the
// user sees in their config.toml file.
func Marshal(cfg *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
