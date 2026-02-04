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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// CertConfig holds certificate management configuration
type CertConfig struct {
	Enabled                bool     `json:"enabled" yaml:"enabled"`
	CASecretName           string   `json:"caSecretName" yaml:"caSecretName"`
	CASecretNamespace      string   `json:"caSecretNamespace" yaml:"caSecretNamespace"`
	ClientSecretName       string   `json:"clientSecretName" yaml:"clientSecretName"`
	ClientSecretNamespaces []string `json:"clientSecretNamespaces" yaml:"clientSecretNamespaces"`
	ValidityYears          int      `json:"validityYears" yaml:"validityYears"`
	Organization           string   `json:"organization" yaml:"organization"`
	CommonName             string   `json:"commonName" yaml:"commonName"`

	// Internal map for fast namespace lookup
	clientSecretNamespaceSet map[string]struct{}
}

func (c *CertConfig) Valid() error {
	if !c.Enabled {
		return nil
	}
	if c.CASecretName == "" {
		return fmt.Errorf("caSecretName is required when cert management is enabled")
	}
	if c.ClientSecretName == "" {
		return fmt.Errorf("clientSecretName is required when cert management is enabled")
	}
	if len(c.ClientSecretNamespaces) == 0 {
		return fmt.Errorf("clientSecretNamespaces is required when cert management is enabled")
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

	// Initialize namespace set for fast lookup
	c.clientSecretNamespaceSet = make(map[string]struct{}, len(c.ClientSecretNamespaces))
	for _, ns := range c.ClientSecretNamespaces {
		c.clientSecretNamespaceSet[ns] = struct{}{}
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

	klog.Infof("Watching for client secret: %s", config.ClientSecretName)
	klog.Infof("Expected namespaces: %v", config.ClientSecretNamespaces)

	ss := &SecretSync{
		config:    config,
		client:    mgr.GetClient(),
		clientset: clientset,
	}

	// Unified predicate for both Secret and Namespace resources
	unifiedPredicate := predicate.NewPredicateFuncs(func(obj ctrclient.Object) bool {
		if _, ok := obj.(*corev1.Namespace); ok {
			return true
		}
		// Handle Secret
		if _, ok := obj.(*corev1.Secret); ok {
			if obj.GetName() != config.ClientSecretName {
				return false
			}
			if _, exists := config.clientSecretNamespaceSet[obj.GetNamespace()]; exists {
				klog.Infof("Secret %s/%s matches expected configuration", obj.GetNamespace(), obj.GetName())
				return true
			}
		}
		return false
	})

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(unifiedPredicate).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj ctrclient.Object) []reconcile.Request {
				ns := obj.GetName()

				// Check if namespace is in the expected set
				if _, exists := config.clientSecretNamespaceSet[ns]; exists {
					req := reconcile.Request{
						NamespacedName: ctrclient.ObjectKey{
							Namespace: ns,
							Name:      config.ClientSecretName,
						},
					}
					return []reconcile.Request{req}
				}
				return nil
			}),
		).
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

	// Generate and ensure client certificates in all namespaces
	// Don't fail if namespaces don't exist yet - they will be created via reconcile
	h.ensureClientCertificates(ctx)

	klog.Info("Certificate initialization completed successfully")
	return nil
}

func (h *SecretSync) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("Reconcile triggered for secret %s/%s", req.Namespace, req.Name)

	// First check if the namespace exists
	var ns corev1.Namespace
	if err := h.client.Get(ctx, ctrclient.ObjectKey{Name: req.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			// Namespace doesn't exist (might be deleted), skip reconciliation
			klog.Infof("Namespace %s not found, skipping secret reconciliation", req.Namespace)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Failed to get namespace %s: %v", req.Namespace, err)
		return ctrl.Result{RequeueAfter: RequeuDelay}, err
	}

	// Check if namespace is being deleted
	if ns.DeletionTimestamp != nil {
		klog.Infof("Namespace %s is being deleted, skipping secret reconciliation", req.Namespace)
		return ctrl.Result{}, nil
	}

	klog.Infof("Namespace %s exists and is active, checking secret", req.Namespace)

	var se corev1.Secret

	err := h.client.Get(ctx, req.NamespacedName, &se)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Secret deleted or doesn't exist yet, recreate it
			if err := h.recreateClientSecret(ctx, req.Namespace); err != nil {
				klog.Errorf("Failed to create client secret: %v", err)
				return ctrl.Result{RequeueAfter: RequeuDelay}, err
			}
			return ctrl.Result{}, nil
		}
		klog.Errorf("Failed to get secret %s/%s: %v", req.Namespace, req.Name, err)
		return ctrl.Result{}, err
	}

	klog.Infof("Secret %s/%s exists, validating certificate", req.Namespace, req.Name)

	// Validate client certificate against CA
	needsRecreation := h.validateClientCert(&se)

	if needsRecreation {
		klog.Infof("Client certificate %s/%s validation failed, recreating", se.Namespace, se.Name)
		if err := h.recreateClientSecret(ctx, se.Namespace); err != nil {
			klog.Errorf("Failed to recreate client secret: %v", err)
			return ctrl.Result{RequeueAfter: RequeuDelay}, err
		}
		klog.Infof("Successfully recreated client certificate %s/%s", se.Namespace, se.Name)
	} else {
		klog.V(4).Infof("Secret %s/%s is valid, no action needed", req.Namespace, req.Name)
	}

	return ctrl.Result{}, nil
}

// ensureClientCertificates creates client certificates in all configured namespaces
func (h *SecretSync) ensureClientCertificates(ctx context.Context) {
	// Generate generic client certificate
	clientCert, err := cert.GenerateClientCert(h.ca, &cert.ClientCertConfig{
		CommonName:   "etcd-client",
		Organization: h.config.Organization,
		DNSNames: []string{
			"127.0.0.1", "localhost",
		},
		IPAddresses:   []string{"127.0.0.1"},
		ValidityYears: h.config.ValidityYears,
	})
	if err != nil {
		klog.Errorf("Failed to generate client certificate: %v", err)
		return
	}

	// Create client secrets in all specified namespaces
	for namespace := range h.config.clientSecretNamespaceSet {
		clientSecretMgr := cert.NewSecretManager(h.clientset, namespace)

		if err := clientSecretMgr.EnsureClientSecret(ctx, h.config.ClientSecretName, h.ca, clientCert); err != nil {
			// Don't fail if namespace doesn't exist - it will be created later
			if apierrors.IsNotFound(err) {
				klog.Warningf("Namespace %s not found, will retry when namespace is created", namespace)
				continue
			}
			klog.Errorf("Failed to create client secret in namespace %s: %v", namespace, err)
			continue
		}

		klog.Infof("Successfully ensured client certificate in secret %s/%s", namespace, h.config.ClientSecretName)
	}
}

// validateClientCert validates that the client certificate matches the CA
func (h *SecretSync) validateClientCert(secret *corev1.Secret) bool {
	// Check if secret has required keys
	caCertPEM, hasCACert := secret.Data[cert.CACertKey]
	clientCertPEM, hasClientCert := secret.Data[cert.ClientCertKey]

	if !hasCACert || !hasClientCert {
		klog.Warningf("Client secret %s/%s missing required keys", secret.Namespace, secret.Name)
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

	// Parse client certificate from secret
	clientCertBlock, _ := pem.Decode(clientCertPEM)
	if clientCertBlock == nil {
		klog.Warningf("Failed to decode client certificate PEM in secret %s/%s", secret.Namespace, secret.Name)
		return true
	}

	clientCert, err := x509.ParseCertificate(clientCertBlock.Bytes)
	if err != nil {
		klog.Warningf("Failed to parse client certificate in secret %s/%s: %v", secret.Namespace, secret.Name, err)
		return true
	}

	// Compare CA certificate in secret with current CA
	if !bytes.Equal(secretCACert.Raw, h.ca.Certificate.Raw) {
		klog.Warningf("CA certificate mismatch in secret %s/%s", secret.Namespace, secret.Name)
		return true
	}

	// Verify client certificate was signed by the CA
	roots := x509.NewCertPool()
	roots.AddCert(h.ca.Certificate)

	opts := x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	if _, err := clientCert.Verify(opts); err != nil {
		klog.Warningf("Client certificate verification failed in secret %s/%s: %v", secret.Namespace, secret.Name, err)
		return true
	}

	return false
}

// recreateClientSecret deletes and recreates a client secret
func (h *SecretSync) recreateClientSecret(ctx context.Context, namespace string) error {
	clientSecretMgr := cert.NewSecretManager(h.clientset, namespace)

	// Delete existing secret
	err := h.clientset.CoreV1().Secrets(namespace).Delete(ctx, h.config.ClientSecretName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete client secret: %w", err)
	}

	// Generate new client certificate
	clientCert, err := cert.GenerateClientCert(h.ca, &cert.ClientCertConfig{
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

	// Create new secret
	if err := clientSecretMgr.EnsureClientSecret(ctx, h.config.ClientSecretName, h.ca, clientCert); err != nil {
		return fmt.Errorf("failed to create client secret: %w", err)
	}

	return nil
}
