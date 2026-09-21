// Package config reads the global config: ~/.config/laatmux/config.yaml.
//
//	hosts:
//	  - name: laptop          # label; omit ssh for this machine
//	  - name: box
//	    ssh: box              # ssh alias, ControlMaster assumed
//	    bin: ~/.local/bin/laatmux   # optional, default "laatmux" on PATH
package config

import (
	"os"
	"path/filepath"

	"github.com/laat/laatmux/internal/client"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Hosts []client.Host `yaml:"hosts"`
}

// Path is the config file location. LAATMUX_CONFIG overrides.
func Path() string {
	if v := os.Getenv("LAATMUX_CONFIG"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "laatmux", "config.yaml")
}

// Load reads the config. A missing file yields one local host named after
// the machine.
func Load() (Config, error) {
	var c Config
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return Config{Hosts: []client.Host{{Name: localName()}}}, nil
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if len(c.Hosts) == 0 {
		c.Hosts = []client.Host{{Name: localName()}}
	}
	for i := range c.Hosts {
		if c.Hosts[i].Name == "" {
			if c.Hosts[i].SSH == "" {
				c.Hosts[i].Name = localName()
			} else {
				c.Hosts[i].Name = c.Hosts[i].SSH
			}
		}
	}
	return c, nil
}

func localName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	if i := indexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// Find returns the host by name, or the local host when name is "".
func (c Config) Find(name string) (client.Host, bool) {
	for _, h := range c.Hosts {
		if name == "" && h.Local() {
			return h, true
		}
		if h.Name == name || (h.SSH != "" && h.SSH == name) {
			return h, true
		}
	}
	if name == "" {
		return client.Host{Name: localName()}, true
	}
	return client.Host{}, false
}
