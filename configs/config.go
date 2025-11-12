package config

import (
    "os"
    "gopkg.in/yaml.v3"
)

// --- Structures for YAML Configuration ---

// AppConfig now includes session configuration and components list.
type AppConfig struct {
    Session    []SessionRequest    `yaml:"session"` // NEW: For Login/Logout endpoints
    Components []ComponentDefinition `yaml:"components"`
}

// SessionRequest defines the configuration for authentication steps (login/logout).
type SessionRequest struct {
    Name     string                 `yaml:"name"`
    Endpoint string                 `yaml:"endpoint"`
    Method   string                 `yaml:"method"`
    Data     map[string]interface{} `yaml:"data"` // Dynamic payload for POST requests
}

// ComponentDefinition groups metrics that share an endpoint
type ComponentDefinition struct {
    Name     string              `yaml:"name"`
    Endpoint string              `yaml:"endpoint"`
    Metrics  []MetricDefinition `yaml:"metrics"`
}

// MetricDefinition defines a single metric endpoint, path, and its labels
type MetricDefinition struct {
    Name     string              `yaml:"name"`
    Help     string              `yaml:"help"`
    Type     string              `yaml:"type"` 
    JsonPath string              `yaml:"json_path"`
    Labels   []LabelDefinition `yaml:"labels"`
}

type LabelDefinition struct {
    Name     string `yaml:"name"`
    JsonPath string `yaml:"json_path"`
}

// --- Loading Function (Stays the same) ---

func LoadConfig(path string) (*AppConfig, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    cfg := &AppConfig{}
    if err := yaml.Unmarshal(data, cfg); err != nil {
        return nil, err
    }
    return cfg, nil
}