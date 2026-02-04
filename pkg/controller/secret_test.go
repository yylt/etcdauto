package controller

import (
	"testing"
)

func TestCertConfig_Valid(t *testing.T) {
	tests := []struct {
		name    string
		config  *CertConfig
		wantErr bool
	}{
		{
			name: "disabled config should pass",
			config: &CertConfig{
				Enabled: false,
			},
			wantErr: false,
		},
		{
			name: "valid config should initialize namespace set",
			config: &CertConfig{
				Enabled:                true,
				CASecretName:           "ca-secret",
				ClientSecretName:       "client-secret",
				ClientSecretNamespaces: []string{"ns1", "ns2", "ns3"},
			},
			wantErr: false,
		},
		{
			name: "missing CA secret name should fail",
			config: &CertConfig{
				Enabled:                true,
				ClientSecretName:       "client-secret",
				ClientSecretNamespaces: []string{"ns1"},
			},
			wantErr: true,
		},
		{
			name: "missing client secret name should fail",
			config: &CertConfig{
				Enabled:                true,
				CASecretName:           "ca-secret",
				ClientSecretNamespaces: []string{"ns1"},
			},
			wantErr: true,
		},
		{
			name: "empty client namespaces should fail",
			config: &CertConfig{
				Enabled:          true,
				CASecretName:     "ca-secret",
				ClientSecretName: "client-secret",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Valid()
			if (err != nil) != tt.wantErr {
				t.Errorf("CertConfig.Valid() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// If validation passed and config is enabled, verify namespace set is initialized
			if err == nil && tt.config.Enabled {
				if tt.config.clientSecretNamespaceSet == nil {
					t.Error("clientSecretNamespaceSet should be initialized after Valid()")
					return
				}

				// Verify all namespaces are in the set
				for _, ns := range tt.config.ClientSecretNamespaces {
					if _, exists := tt.config.clientSecretNamespaceSet[ns]; !exists {
						t.Errorf("namespace %s should be in clientSecretNamespaceSet", ns)
					}
				}

				// Verify set size matches slice length
				if len(tt.config.clientSecretNamespaceSet) != len(tt.config.ClientSecretNamespaces) {
					t.Errorf("clientSecretNamespaceSet size = %d, want %d",
						len(tt.config.clientSecretNamespaceSet), len(tt.config.ClientSecretNamespaces))
				}
			}
		})
	}
}

func TestCertConfig_NamespaceSetLookup(t *testing.T) {
	config := &CertConfig{
		Enabled:                true,
		CASecretName:           "ca-secret",
		ClientSecretName:       "client-secret",
		ClientSecretNamespaces: []string{"ns1", "ns2", "ns3"},
	}

	if err := config.Valid(); err != nil {
		t.Fatalf("Valid() failed: %v", err)
	}

	tests := []struct {
		namespace string
		wantFound bool
	}{
		{"ns1", true},
		{"ns2", true},
		{"ns3", true},
		{"ns4", false},
		{"default", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			_, found := config.clientSecretNamespaceSet[tt.namespace]
			if found != tt.wantFound {
				t.Errorf("namespace %s: found = %v, want %v", tt.namespace, found, tt.wantFound)
			}
		})
	}
}

func TestCertConfig_DefaultValues(t *testing.T) {
	config := &CertConfig{
		Enabled:                true,
		CASecretName:           "ca-secret",
		ClientSecretName:       "client-secret",
		ClientSecretNamespaces: []string{"ns1"},
	}

	if err := config.Valid(); err != nil {
		t.Fatalf("Valid() failed: %v", err)
	}

	if config.ValidityYears != 100 {
		t.Errorf("ValidityYears = %d, want 100", config.ValidityYears)
	}

	if config.Organization != "etcdauto" {
		t.Errorf("Organization = %s, want etcdauto", config.Organization)
	}

	if config.CommonName != "etcdauto-ca" {
		t.Errorf("CommonName = %s, want etcdauto-ca", config.CommonName)
	}
}
