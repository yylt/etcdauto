package cert

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// Secret data keys
	CACertKey     = "ca.pem"
	CAKeyKey      = "ca-key.pem"
	MemberCertKey = "member.pem"
	MemberKeyKey  = "member-key.pem"

	// Annotation keys for certificate metadata
	AnnotationCreatedAt    = "etcdauto.io/cert-created-at"
	AnnotationExpiresAt    = "etcdauto.io/cert-expires-at"
	AnnotationRotationFlag = "etcdauto.io/cert-rotation-enabled"
)

// SecretManager manages certificate secrets in Kubernetes
type SecretManager struct {
	client    kubernetes.Interface
	namespace string
}

// NewSecretManager creates a new SecretManager
func NewSecretManager(client kubernetes.Interface, namespace string) *SecretManager {
	return &SecretManager{
		client:    client,
		namespace: namespace,
	}
}

// EnsureCASecret ensures CA certificate secret exists
// If the secret already exists, it will NOT be updated or deleted
func (sm *SecretManager) EnsureCASecret(ctx context.Context, secretName string, ca *CACertificate) error {
	// Check if secret already exists
	existing, err := sm.client.CoreV1().Secrets(sm.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		// Secret exists, do not update
		klog.Infof("CA secret %s/%s already exists, skipping creation", sm.namespace, secretName)

		// Verify the existing secret has required keys
		if _, ok := existing.Data[CACertKey]; !ok {
			return fmt.Errorf("existing CA secret missing %s key", CACertKey)
		}
		if _, ok := existing.Data[CAKeyKey]; !ok {
			return fmt.Errorf("existing CA secret missing %s key", CAKeyKey)
		}

		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check CA secret: %w", err)
	}

	// Secret doesn't exist, create it
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: sm.namespace,
			Annotations: map[string]string{
				AnnotationCreatedAt:    time.Now().Format(time.RFC3339),
				AnnotationExpiresAt:    ca.Certificate.NotAfter.Format(time.RFC3339),
				AnnotationRotationFlag: "false", // Reserved for future rotation feature
			},
			Labels: map[string]string{
				"app.kubernetes.io/name":      "etcd",
				"app.kubernetes.io/component": "ca-certificate",
				"app.kubernetes.io/managed-by": "etcdauto",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			CACertKey: ca.CertPEM,
			CAKeyKey:  ca.KeyPEM,
		},
	}

	_, err = sm.client.CoreV1().Secrets(sm.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create CA secret: %w", err)
	}

	klog.Infof("Successfully created CA secret %s/%s", sm.namespace, secretName)
	return nil
}

// EnsureMemberSecret ensures member certificate secret exists
// If the secret already exists, it will NOT be updated or deleted
func (sm *SecretManager) EnsureMemberSecret(ctx context.Context, secretName string, ca *CACertificate, member *MemberCertificate) error {
	// Check if secret already exists
	existing, err := sm.client.CoreV1().Secrets(sm.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		// Secret exists, do not update
		klog.Infof("Member secret %s/%s already exists, skipping creation", sm.namespace, secretName)

		// Verify the existing secret has required keys
		requiredKeys := []string{CACertKey, MemberCertKey, MemberKeyKey}
		for _, key := range requiredKeys {
			if _, ok := existing.Data[key]; !ok {
				return fmt.Errorf("existing member secret missing %s key", key)
			}
		}

		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check member secret: %w", err)
	}

	// Secret doesn't exist, create it
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: sm.namespace,
			Annotations: map[string]string{
				AnnotationCreatedAt:    time.Now().Format(time.RFC3339),
				AnnotationExpiresAt:    member.Certificate.NotAfter.Format(time.RFC3339),
				AnnotationRotationFlag: "false", // Reserved for future rotation feature
			},
			Labels: map[string]string{
				"app.kubernetes.io/name":       "etcd",
				"app.kubernetes.io/component":  "member-certificate",
				"app.kubernetes.io/managed-by": "etcdauto",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			CACertKey:     ca.CertPEM,
			MemberCertKey: member.CertPEM,
			MemberKeyKey:  member.KeyPEM,
			// Also add standard TLS secret keys for compatibility
			corev1.TLSCertKey:       member.CertPEM,
			corev1.TLSPrivateKeyKey: member.KeyPEM,
		},
	}

	_, err = sm.client.CoreV1().Secrets(sm.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create member secret: %w", err)
	}

	klog.Infof("Successfully created member secret %s/%s", sm.namespace, secretName)
	return nil
}

// LoadCAFromSecret loads CA certificate from a secret
func (sm *SecretManager) LoadCAFromSecret(ctx context.Context, secretName string) (*CACertificate, error) {
	secret, err := sm.client.CoreV1().Secrets(sm.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get CA secret: %w", err)
	}

	certPEM, ok := secret.Data[CACertKey]
	if !ok {
		return nil, fmt.Errorf("CA secret missing %s key", CACertKey)
	}

	keyPEM, ok := secret.Data[CAKeyKey]
	if !ok {
		return nil, fmt.Errorf("CA secret missing %s key", CAKeyKey)
	}

	return LoadCA(certPEM, keyPEM)
}

// CheckCertificateRotationNeeded checks if certificate rotation is needed
// This is a placeholder for future certificate rotation feature
func (sm *SecretManager) CheckCertificateRotationNeeded(ctx context.Context, secretName string, threshold time.Duration) (bool, error) {
	secret, err := sm.client.CoreV1().Secrets(sm.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get secret: %w", err)
	}

	// Check if rotation is enabled
	if secret.Annotations[AnnotationRotationFlag] != "true" {
		return false, nil
	}

	// Parse expiration time
	expiresAtStr, ok := secret.Annotations[AnnotationExpiresAt]
	if !ok {
		return false, fmt.Errorf("secret missing expiration annotation")
	}

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	if err != nil {
		return false, fmt.Errorf("failed to parse expiration time: %w", err)
	}

	// Check if certificate is expiring soon
	return time.Until(expiresAt) < threshold, nil
}

// EnableCertificateRotation enables certificate rotation for a secret
// This is a placeholder for future certificate rotation feature
func (sm *SecretManager) EnableCertificateRotation(ctx context.Context, secretName string) error {
	secret, err := sm.client.CoreV1().Secrets(sm.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations[AnnotationRotationFlag] = "true"

	_, err = sm.client.CoreV1().Secrets(sm.namespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update secret: %w", err)
	}

	klog.Infof("Enabled certificate rotation for secret %s/%s", sm.namespace, secretName)
	return nil
}
