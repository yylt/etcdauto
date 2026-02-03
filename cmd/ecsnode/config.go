package main

import (
	"fmt"
	"os"

	"github.com/yylt/etcdauto/pkg/controller"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Cert          controller.CertConfig      `json:"cert" yaml:"cert"`
	Configmap     controller.ConfigMapConfig `json:"configmap" yaml:"configmap"`
	PodConfig     controller.PodConfig       `json:"pod" yaml:"pod"`
	EcsNode       controller.EcsNodeConfig   `json:"ecsnode" yaml:"ecsnode"`
	ServiceConfig controller.ServiceConfig   `json:"service" yaml:"service"`
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
		// Ensure CA namespace is in member namespaces
		found := false
		for _, ns := range newcfg.Cert.MemberSecretNamespaces {
			if ns == newcfg.Cert.CASecretNamespace {
				found = true
				break
			}
		}
		if !found {
			newcfg.Cert.MemberSecretNamespaces = append(newcfg.Cert.MemberSecretNamespaces, newcfg.Cert.CASecretNamespace)
		}
		if err := newcfg.Cert.Valid(); err != nil {
			return fmt.Errorf("cert config is invalid: %w", err)
		}
	}

	if newcfg.Configmap.Name != "" {
		if newcfg.Configmap.Namespace == "" {
			newcfg.Configmap.Namespace = defaultns
		}
		if newcfg.Configmap.Valid() != nil {
			return fmt.Errorf("configmap is invalid: %v", newcfg.Configmap)
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
	if newcfg.PodConfig.Namespace == "" {
		newcfg.PodConfig.Namespace = defaultns
	}
	if newcfg.EcsNode.Namespace == "" {
		newcfg.EcsNode.Namespace = defaultns
	}
	switch {
	case newcfg.EcsNode.Valid() != nil:
		return fmt.Errorf("invalid ecnsnode config: %v, failed: %w", newcfg.EcsNode, newcfg.EcsNode.Valid())
	case newcfg.PodConfig.Valid() != nil:
		return fmt.Errorf("invalid pod config: %v, failed: %w", newcfg.PodConfig, newcfg.PodConfig.Valid())
	}
	return nil
}
