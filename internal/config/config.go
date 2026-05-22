package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type RepoConfig struct {
	Owner          string   `yaml:"owner"`
	Repo           string   `yaml:"repo"`
	DefaultBranch  string   `yaml:"default_branch,omitempty"`
	Description    string   `yaml:"description,omitempty"`
	Homepage       string   `yaml:"homepage,omitempty"`
	Visibility     string   `yaml:"visibility,omitempty"`
	Language       string   `yaml:"language,omitempty"`
	License        string   `yaml:"license,omitempty"`
	Topics         []string `yaml:"topics,omitempty"`
	Archived       bool     `yaml:"archived"`
	HasIssues      bool     `yaml:"has_issues"`
	HasProjects    bool     `yaml:"has_projects"`
	HasWiki        bool     `yaml:"has_wiki"`
	HasPages       bool     `yaml:"has_pages"`
	HasDiscussions bool     `yaml:"has_discussions"`
	CreatedAt      string   `yaml:"created_at,omitempty"`
	UpdatedAt      string   `yaml:"updated_at,omitempty"`
	PushedAt       string   `yaml:"pushed_at,omitempty"`
	SyncedAt       string   `yaml:"synced_at,omitempty"`
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
