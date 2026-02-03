package controller

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/yylt/etcdauto/pkg/cert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// CertConfig holds certificate management configuration
type CertConfig struct {
	Enabled                bool     `json:"enabled" yaml:"enabled"`
	CASecretName           string   `json:"caSecretName" yaml:"caSecretName"`
	CASecretNamespace      string   `json:"caSecretNamespace" yaml:"caSecretNamespace"`
	MemberSecretName       string   `json:"memberSecretName" yaml:"memberSecretName"`
	MemberSecretNamespaces []string `json:"memberSecretNamespaces" yaml:"memberSecretNamespaces"`
	ValidityYears          int      `json:"validityYears" yaml:"validityYears"`
	Organization           string   `json:"organization" yaml:"organization"`
	CommonName             string   `json:"commonName" yaml:"commonName"`
}

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
		c.ValidityYears = 100
	}
	if c.Organization == "" {
		c.Organization = "etcdauto"
	}
	if c.CommonName == "" {
		c.CommonName = "etcdauto-ca"
	}
	return nil
}

type SecretSync struct {
	config    *CertConfig
	client    ctrclient.Client
	clientset kubernetes.Interface
	ca        *cert.CACertificate
}

func NewSecretSync(config *CertConfig, mgr manager.Manager, clientset kubernetes.Interface) *SecretSync {
	if !config.Enabled {
		return nil
	}

	ss := &SecretSync{
		config:    config,
		client:    mgr.GetClient(),
		clientset: clientset,
	}

	// Watch member secrets in all configured namespaces
	predicateFn := predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
		if object.GetName() != config.MemberSecretName {
			return false
		}
		for _, ns := range config.MemberSecretNamespaces {
			if object.GetNamespace() == ns {
				return true
			}
		}
		return false
	})

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(predicateFn).
		Complete(ss)
	if err != nil {
		klog.Fatalf("create secret controller failed: %v", err)
	}
	return ss
}

func (h *SecretSync) NeedLeaderElection() bool { return true }

func (h *SecretSync) Start(ctx context.Context) error {
	klog.Info("Certificate management is enabled, initializing certificates...")

	// Create secret manager for CA
	caSecretMgr := cert.NewSecretManager(h.clientset, h.config.CASecretNamespace)

	// Try to load existing CA certificate
	ca, err := caSecretMgr.LoadCAFromSecret(ctx, h.config.CASecretName)
	if err != nil {
		klog.Infof("CA certificate not found, generating new CA: %v", err)

		// Generate new CA certificate
		ca, err = cert.GenerateCA(&cert.CAConfig{
			CommonName:    h.config.CommonName,
			Organization:  h.config.Organization,
			ValidityYears: h.config.ValidityYears,
		})
		if err != nil {
			return fmt.Errorf("failed to generate CA certificate: %w", err)
		}

		// Create CA secret
		if err := caSecretMgr.EnsureCASecret(ctx, h.config.CASecretName, ca); err != nil {
			return fmt.Errorf("failed to create CA secret: %w", err)
		}

		klog.Infof("Successfully created CA certificate in secret %s/%s", h.config.CASecretNamespace, h.config.CASecretName)
	} else {
		klog.Infof("Loaded existing CA certificate from secret %s/%s", h.config.CASecretNamespace, h.config.CASecretName)
	}

	// Store CA for later use
	h.ca = ca

	// Generate and ensure member certificates in all namespaces
	if err := h.ensureMemberCertificates(ctx); err != nil {
		return fmt.Errorf("failed to ensure member certificates: %w", err)
	}

	klog.Info("Certificate initialization completed successfully")
	return nil
}

func (h *SecretSync) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var se corev1.Secret

	err := h.client.Get(ctx, req.NamespacedName, &se)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Secret deleted, recreate it
			klog.Infof("Member secret %s/%s not found, recreating", req.Namespace, req.Name)
			if err := h.recreateMemberSecret(ctx, req.Namespace); err != nil {
				klog.Errorf("Failed to recreate member secret: %v", err)
				return ctrl.Result{RequeueAfter: RequeuDelay}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Validate member certificate against CA
	needsRecreation := h.validateMemberCert(&se)

	if needsRecreation {
		klog.Infof("Member certificate %s/%s validation failed, recreating", se.Namespace, se.Name)
		if err := h.recreateMemberSecret(ctx, se.Namespace); err != nil {
			klog.Errorf("Failed to recreate member secret: %v", err)
			return ctrl.Result{RequeueAfter: RequeuDelay}, err
		}
		klog.Infof("Successfully recreated member certificate %s/%s", se.Namespace, se.Name)
	}

	return ctrl.Result{}, nil
}

// ensureMemberCertificates creates member certificates in all configured namespaces
func (h *SecretSync) ensureMemberCertificates(ctx context.Context) error {
	// Generate generic client certificate
	clientCert, err := cert.GenerateMemberCert(h.ca, &cert.MemberCertConfig{
		CommonName:   "etcd-client",
		Organization: h.config.Organization,
		DNSNames: []string{
			"127.0.0.1", "localhost",
		},
		IPAddresses:   []string{"127.0.0.1"},
		ValidityYears: h.config.ValidityYears,
	})
	if err != nil {
		return fmt.Errorf("failed to generate client certificate: %w", err)
	}

	// Create member secrets in all specified namespaces
	for _, namespace := range h.config.MemberSecretNamespaces {
		memberSecretMgr := cert.NewSecretManager(h.clientset, namespace)

		if err := memberSecretMgr.EnsureMemberSecret(ctx, h.config.MemberSecretName, h.ca, clientCert); err != nil {
			return fmt.Errorf("failed to create member secret in namespace %s: %w", namespace, err)
		}

		klog.Infof("Successfully ensured member certificate in secret %s/%s", namespace, h.config.MemberSecretName)
	}

	return nil
}

// validateMemberCert validates that the member certificate matches the CA
func (h *SecretSync) validateMemberCert(secret *corev1.Secret) bool {
	// Check if secret has required keys
	caCertPEM, hasCACert := secret.Data[cert.CACertKey]
	memberCertPEM, hasMemberCert := secret.Data[cert.MemberCertKey]

	if !hasCACert || !hasMemberCert {
		klog.Warningf("Member secret %s/%s missing required keys", secret.Namespace, secret.Name)
		return true
	}

	// Parse CA certificate from secret
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		klog.Warningf("Failed to decode CA certificate PEM in secret %s/%s", secret.Namespace, secret.Name)
		return true
	}

	secretCACert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		klog.Warningf("Failed to parse CA certificate in secret %s/%s: %v", secret.Namespace, secret.Name, err)
		return true
	}

	// Parse member certificate from secret
	memberCertBlock, _ := pem.Decode(memberCertPEM)
	if memberCertBlock == nil {
		klog.Warningf("Failed to decode member certificate PEM in secret %s/%s", secret.Namespace, secret.Name)
		return true
	}

	memberCert, err := x509.ParseCertificate(memberCertBlock.Bytes)
	if err != nil {
		klog.Warningf("Failed to parse member certificate in secret %s/%s: %v", secret.Namespace, secret.Name, err)
		return true
	}

	// Compare CA certificate in secret with current CA
	if !bytes.Equal(secretCACert.Raw, h.ca.Certificate.Raw) {
		klog.Warningf("CA certificate mismatch in secret %s/%s", secret.Namespace, secret.Name)
		return true
	}

	// Verify member certificate was signed by the CA
	roots := x509.NewCertPool()
	roots.AddCert(h.ca.Certificate)

	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	if _, err := memberCert.Verify(opts); err != nil {
		klog.Warningf("Member certificate verification failed in secret %s/%s: %v", secret.Namespace, secret.Name, err)
		return true
	}

	return false
}

// recreateMemberSecret deletes and recreates a member secret
func (h *SecretSync) recreateMemberSecret(ctx context.Context, namespace string) error {
	memberSecretMgr := cert.NewSecretManager(h.clientset, namespace)

	// Delete existing secret
	err := h.clientset.CoreV1().Secrets(namespace).Delete(ctx, h.config.MemberSecretName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete member secret: %w", err)
	}

	// Generate new member certificate
	clientCert, err := cert.GenerateMemberCert(h.ca, &cert.MemberCertConfig{
		CommonName:   "etcd-client",
		Organization: h.config.Organization,
		DNSNames: []string{
			"127.0.0.1", "localhost",
		},
		IPAddresses:   []string{"127.0.0.1"},
		ValidityYears: h.config.ValidityYears,
	})
	if err != nil {
		return fmt.Errorf("failed to generate member certificate: %w", err)
	}

	// Create new secret
	if err := memberSecretMgr.EnsureMemberSecret(ctx, h.config.MemberSecretName, h.ca, clientCert); err != nil {
		return fmt.Errorf("failed to create member secret: %w", err)
	}

	return nil
}
