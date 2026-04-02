package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type RepoConfig struct {
	Owner         string `yaml:"owner"`
	Repo          string `yaml:"repo"`
	DefaultBranch string `yaml:"default_branch,omitempty"`
	SyncedAt      string `yaml:"synced_at,omitempty"`
}

func ReadRepoConfig(path string) (*RepoConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg RepoConfig
	return &cfg, yaml.Unmarshal(data, &cfg)
}

func WriteRepoConfig(path string, cfg *RepoConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
