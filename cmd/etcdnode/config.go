package main

import (
	"fmt"
	"os"

	"github.com/yylt/etcdauto/pkg/controller"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Cert          controller.CertConfig     `json:"cert" yaml:"cert"`
	ServiceConfig controller.ServiceConfig  `json:"service" yaml:"service"`
	NodeConfig    controller.NodeConfig     `json:"node" yaml:"node"`
	NodeSync      controller.NodeSyncConfig `json:"nodesync" yaml:"nodesync"`
}

// LoadConfigmap reads data from file-path
func LoadFromYaml(fp string) (*Config, error) {
	var (
		cfg = &Config{}
	)
	configmapBytes, err := os.ReadFile(fp)
	if nil != err {
		return nil, fmt.Errorf("failed to read config file %s, error: %w", fp, err)
	}

	err = yaml.Unmarshal(configmapBytes, &cfg)
	if nil != err {
		return nil, fmt.Errorf("failed to parse configmap, error: %w", err)
	}

	return cfg, nil
}

func ApplyDefault(newcfg *Config, defaultns string) error {
	if newcfg == nil {
		return fmt.Errorf("config is nil")
	}

	// Apply defaults for certificate config
	if newcfg.Cert.Enabled {
		if newcfg.Cert.CASecretNamespace == "" {
			newcfg.Cert.CASecretNamespace = defaultns
		}
		// Ensure CA namespace is in client namespaces
		found := false
		for _, ns := range newcfg.Cert.ClientSecretNamespaces {
			if ns == newcfg.Cert.CASecretNamespace {
				found = true
				break
			}
		}
		if !found {
			newcfg.Cert.ClientSecretNamespaces = append(newcfg.Cert.ClientSecretNamespaces, newcfg.Cert.CASecretNamespace)
		}
		// Validate will initialize the namespace set
		if err := newcfg.Cert.Valid(); err != nil {
			return fmt.Errorf("cert config is invalid: %w", err)
		}
	}

	if newcfg.ServiceConfig.Name != "" {
		if newcfg.ServiceConfig.Namespace == "" {
			newcfg.ServiceConfig.Namespace = defaultns
		}
		if newcfg.ServiceConfig.Valid() != nil {
			return fmt.Errorf("service is invalid: %v", newcfg.ServiceConfig)
		}
	}
	switch {
	case len(newcfg.NodeConfig.Interfaces) > 0 && newcfg.NodeConfig.Valid() != nil:
		return fmt.Errorf("invalid node config: %v, failed: %w", newcfg.NodeConfig, newcfg.NodeConfig.Valid())
	case newcfg.NodeSync.ConfigMapName != "" && newcfg.NodeSync.Valid() != nil:
		return fmt.Errorf("invalid nodesync config: %v, failed: %w", newcfg.NodeSync, newcfg.NodeSync.Valid())
	}
	return nil
}
