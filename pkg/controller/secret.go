package controller

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	cfsslconfig "github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/csr"
	cfssllog "github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type SecretConfig struct {
	Name string `json:"name" yaml:"name"`
	// secret namespace
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Profile   struct {
		Expirey string   `json:"expirey,omitempty" yaml:"expirey,omitempty"`
		Usage   []string `json:"usage,omitempty" yaml:"usage,omitempty"`
	} `json:"profile,omitempty" yaml:"profile,omitempty"`

	CaCrtFile string `json:"ca_crtfile,omitempty" yaml:"ca_crtfile,omitempty"`
	CaKeyFile string `json:"ca_keyfile,omitempty" yaml:"ca_keyfile,omitempty"`
}

var (
	keyName   = "tls.key"
	crtName   = "tls.crt"
	cacrtName = "ca.crt"

	keydict = map[string]struct{}{
		keyName:   {},
		crtName:   {},
		cacrtName: {},
	}
)

const (
	cn = "cli.ecsnode.io"
)

func init() {
	cfssllog.Level = cfssllog.LevelError
}

func (c *SecretConfig) Valid() error {
	if len(c.Namespace) == 0 {
		return fmt.Errorf("namespace must not be empty")
	}
	if len(c.CaCrtFile) == 0 || len(c.CaKeyFile) == 0 {
		return fmt.Errorf("ca_crtfile and ca_keyfile must not be empty")
	}
	return nil
}

type SecretSync struct {
	config *SecretConfig
	client ctrclient.Client

	sign signer.Signer
}

func NewSecretSync(config *SecretConfig, mgr manager.Manager) *SecretSync {
	expiry, err := time.ParseDuration(config.Profile.Expirey)
	if err != nil {
		panic(err)
	}
	signprofile := &cfsslconfig.Signing{
		Profiles: map[string]*cfsslconfig.SigningProfile{},
		Default: &cfsslconfig.SigningProfile{
			Usage:        config.Profile.Usage,
			ExpiryString: config.Profile.Expirey,
			Expiry:       expiry,
		},
	}
	sign, err := local.NewSignerFromFile(config.CaCrtFile, config.CaKeyFile, signprofile)
	if err != nil {
		panic(err)
	}
	ss := &SecretSync{
		config: config,
		client: mgr.GetClient(),
		sign:   sign,
	}
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object ctrclient.Object) bool {
			if object.GetName() == config.Name && object.GetNamespace() == config.Namespace {
				return true
			}
			return false
		})).
		Complete(ss)
	if err != nil {
		klog.Fatalf("create secret controller failed: %v", err)
	}
	return ss
}

func (h *SecretSync) NeedLeaderElection() bool { return true }

func (h *SecretSync) Start(ctx context.Context) error {
	result, err := h.genSecret(ctx)
	if err != nil {
		return err
	}
	klog.Infof("Secret started, result: %s", result)
	return nil
}

func (h *SecretSync) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		se = &corev1.Secret{}
	)
	updatefn := func() (ctrl.Result, error) {
		result, err := h.genSecret(ctx)
		if err != nil {
			klog.Errorf("gen secret failed, err: %s", err)
			return ctrl.Result{RequeueAfter: RequeuDelay}, err
		}
		klog.Infof("Secret updated, result: %s", result)
		return ctrl.Result{}, nil
	}
	err := h.client.Get(ctx, req.NamespacedName, se)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return updatefn()
		}
	}
	if len(se.Data) != len(keydict) {
		return updatefn()
	}

	for k := range keydict {
		if _, ok := se.Data[k]; !ok {
			return updatefn()
		}
	}

	ca, err := os.ReadFile(h.config.CaCrtFile)
	if err != nil {
		klog.Errorf("read file '%s' failed, err: %s", h.config.CaCrtFile, err)
		return updatefn()
	}

	if !bytes.Equal(se.Data[cacrtName], ca) {
		return updatefn()
	}
	return ctrl.Result{}, nil
}

func (h *SecretSync) genSecret(ctx context.Context) (controllerutil.OperationResult, error) {
	var (
		se = &corev1.Secret{}
	)
	se.Name = h.config.Name
	se.Namespace = h.config.Namespace
	se.Type = corev1.SecretTypeOpaque

	csr, key, err := csr.ParseRequest(csr.New())
	if err != nil {
		return controllerutil.OperationResultNone, err
	}
	cert, err := h.sign.Sign(signer.SignRequest{Request: string(csr), Hosts: signer.SplitHosts(cn)})
	if err != nil {
		return controllerutil.OperationResultNone, err
	}
	ca, _ := os.ReadFile(h.config.CaCrtFile)
	se.Data = map[string][]byte{
		cacrtName: ca,
		crtName:   cert,
		keyName:   key,
	}
	return controllerutil.CreateOrUpdate(ctx, h.client, se, nil)
}
