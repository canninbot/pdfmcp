// Package config defines application limits and filesystem configuration.
package config

import (
	"fmt"
	"path/filepath"
)

const (
	DefaultTextLimit      = 200_000
	DefaultImageLimit     = 20 << 20
	DefaultQueryLimit     = 20
	MaxDetailedQueryLimit = 200
	MaxPMIDQueryLimit     = 5_000
	MaxPMIDResponseBytes  = 1 << 20
	// MaxQueryLimit remains the browsing and detailed-output limit.
	MaxQueryLimit   = MaxDetailedQueryLimit
	MaxContextLines = 20
	SchemaVersion   = 8
)

// Config contains the corpus and database locations used by all commands.
type Config struct {
	Root          string
	Database      string
	TextLimit     int
	ImageLimit    int64
	QueryTimeout  int
	SyncOnStartup bool
}

// New resolves and validates configuration rooted at root.
func New(root, database string) (Config, error) {
	if root == "" {
		return Config{}, fmt.Errorf("corpus root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve corpus root: %w", err)
	}
	if database == "" {
		database = filepath.Join(absRoot, "papers.sqlite")
	} else if !filepath.IsAbs(database) {
		database = filepath.Join(absRoot, database)
	}
	absDatabase, err := filepath.Abs(database)
	if err != nil {
		return Config{}, fmt.Errorf("resolve database path: %w", err)
	}
	return Config{
		Root:         filepath.Clean(absRoot),
		Database:     filepath.Clean(absDatabase),
		TextLimit:    DefaultTextLimit,
		ImageLimit:   DefaultImageLimit,
		QueryTimeout: 30,
	}, nil
}
