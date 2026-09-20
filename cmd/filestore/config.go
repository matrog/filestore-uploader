package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the settings resolved from the environment and the config file.
type Config struct {
	Key  string
	Site string
}

func configPath() string {
	if p := os.Getenv("FILESTORE_CONF"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".filestore.conf"
	}
	return filepath.Join(home, ".filestore.conf")
}

// siteURL allows pointing at a different host (used by the end-to-end tests).
func siteURL() string {
	if s := os.Getenv("FILESTORE_SITE"); s != "" {
		return strings.TrimRight(s, "/")
	}
	return "https://filestore.me"
}

func loadConfig() (*Config, error) {
	c := &Config{Site: siteURL()}
	if k := os.Getenv("FILESTORE_API_KEY"); k != "" {
		c.Key = strings.TrimSpace(k)
		return c, nil
	}
	f, err := os.Open(configPath())
	if err != nil {
		return nil, fmt.Errorf("no API key configured: run \"filestore setup\"")
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == "FILESTORE_API_KEY" {
			c.Key = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if c.Key == "" {
		return nil, fmt.Errorf("no API key found in %s: run \"filestore setup\"", configPath())
	}
	return c, nil
}

func saveKey(key string) error {
	p := configPath()
	// 0600: the key stays readable by this user only.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "FILESTORE_API_KEY=%s\n", key); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}
