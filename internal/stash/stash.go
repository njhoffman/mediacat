// Package stash reads Stash's configuration, read-only, to tell when mediacat changed
// files Stash tracks.
package stash

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfig is Stash's default configuration path.
func DefaultConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".stash", "config.yml")
}

// Libraries returns the library paths in a Stash config. A missing config yields none.
func Libraries(configPath string) ([]string, error) {
	b, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Stash []struct {
			Path string `yaml:"path"`
		} `yaml:"stash"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range cfg.Stash {
		if s.Path != "" {
			out = append(out, filepath.Clean(s.Path))
		}
	}
	return out, nil
}

// Affected returns the paths that lie inside any library.
func Affected(paths, libraries []string) []string {
	var out []string
	for _, p := range paths {
		for _, lib := range libraries {
			if p == lib || strings.HasPrefix(p, lib+"/") {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
