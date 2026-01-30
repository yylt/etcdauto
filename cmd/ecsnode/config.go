package main

import (
	"fmt"
	"os"

	"github.com/yylt/etcdauto/pkg/controller"

	"gopkg.in/yaml.v3"
)

// CertConfig holds certificate management configuration
type CertConfig struct {
	Enabled                bool     `json:"enabled" yaml:"enabled"`                               // Enable certificate management
	CASecretName           string   `json:"caSecretName" yaml:"caSecretName"`                     // CA certificate secret name
	CASecretNamespace      string   `json:"caSecretNamespace" yaml:"caSecretNamespace"`           // CA certificate secret namespace
	MemberSecretName       string   `json:"memberSecretName" yaml:"memberSecretName"`             // Member certificate secret name
	MemberSecretNamespaces []string `json:"memberSecretNamespaces" yaml:"memberSecretNamespaces"` // Member certificate secret namespaces (list)
	ValidityYears          int      `json:"validityYears" yaml:"validityYears"`                   // Certificate validity in years (default: 100)
	Organization           string   `json:"organization" yaml:"organization"`                     // Organization name for certificates
	CommonName             string   `json:"commonName" yaml:"commonName"`                         // Common name for CA certificate
}

// Valid validates certificate configuration
func (c *CertConfig) Valid() error {
	if !c.Enabled {
		return nil
	}
	if c.CASecretName == "" {
		return fmt.Errorf("caSecretName is required when cert management is enabled")
	}
	if c.MemberSecretName == "" {
		return fmt.Errorf("memberSecretName is required when cert management is enabled")
	}
	if len(c.MemberSecretNamespaces) == 0 {
		return fmt.Errorf("memberSecretNamespaces is required when cert management is enabled")
	}
	if c.ValidityYears <= 0 {
		c.ValidityYears = 100 // Default to 100 years
	}
	if c.Organization == "" {
		c.Organization = "etcdauto"
	}
	if c.CommonName == "" {
		c.CommonName = "etcdauto-ca"
	}
	return nil
}

type Config struct {
	Cert          CertConfig                 `json:"cert" yaml:"cert"`
	Configmap     controller.ConfigMapConfig `json:"configmap" yaml:"configmap"`
	PodConfig     controller.PodConfig       `json:"pod" yaml:"pod"`
	EcsNode       controller.EcsNodeConfig   `json:"ecsnode" yaml:"ecsnode"`
	ServiceConfig controller.ServiceConfig   `json:"service" yaml:"service"`
	Secret        controller.SecretConfig    `json:"secret" yaml:"secret"`
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
		// If no member secret namespaces specified, use default namespace
		if len(newcfg.Cert.MemberSecretNamespaces) == 0 {
			newcfg.Cert.MemberSecretNamespaces = []string{defaultns}
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
	if newcfg.Secret.Name != "" {
		if newcfg.Secret.Namespace == "" {
			newcfg.Secret.Namespace = defaultns
		}
		if newcfg.Secret.Valid() != nil {
			return fmt.Errorf("secret is invalid: %v", newcfg.Secret)
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
