package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNodeController_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	testNodeName := "test-node"
	os.Setenv("NODE_NAME", testNodeName)
	defer os.Unsetenv("NODE_NAME")

	tests := []struct {
		name              string
		node              *corev1.Node
		interfaces        []string
		expectUpdate      bool
		expectAnnotations map[string]bool // map of annotation keys that should exist
	}{
		{
			name: "add annotations to node without annotations",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNodeName,
				},
			},
			interfaces:   []string{"lo"}, // loopback interface should exist on all systems
			expectUpdate: true,
			expectAnnotations: map[string]bool{
				NodeAnnotationPrefix + "lo": true,
			},
		},
		{
			name: "skip when annotations already exist",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNodeName,
					Annotations: map[string]string{
						NodeAnnotationPrefix + "lo": "127.0.0.1",
					},
				},
			},
			interfaces:   []string{"lo"},
			expectUpdate: false,
			expectAnnotations: map[string]bool{
				NodeAnnotationPrefix + "lo": true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with the node
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.node).
				Build()

			config := &NodeConfig{
				Interfaces: tt.interfaces,
			}

			nodeCtrl := &NodeCtrl{
				config:   config,
				client:   fakeClient,
				nodeName: testNodeName,
			}

			// Reconcile
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: testNodeName,
				},
			}

			_, err := nodeCtrl.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			// Get updated node
			updatedNode := &corev1.Node{}
			err = fakeClient.Get(context.Background(), types.NamespacedName{Name: testNodeName}, updatedNode)
			if err != nil {
				t.Fatalf("Failed to get updated node: %v", err)
			}

			// Check annotations
			if updatedNode.Annotations == nil {
				t.Fatal("Node annotations are nil")
			}

			for key, shouldExist := range tt.expectAnnotations {
				_, exists := updatedNode.Annotations[key]
				if shouldExist && !exists {
					t.Errorf("Expected annotation %s to exist, but it doesn't", key)
				}
				if !shouldExist && exists {
					t.Errorf("Expected annotation %s to not exist, but it does", key)
				}
			}
		})
	}
}

func TestNodeConfig_Valid(t *testing.T) {
	tests := []struct {
		name      string
		config    *NodeConfig
		expectErr bool
	}{
		{
			name: "valid config",
			config: &NodeConfig{
				Interfaces: []string{"eth0", "eth1"},
			},
			expectErr: false,
		},
		{
			name: "empty interfaces",
			config: &NodeConfig{
				Interfaces: []string{},
			},
			expectErr: true,
		},
		{
			name: "nil interfaces",
			config: &NodeConfig{
				Interfaces: nil,
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Valid()
			if tt.expectErr && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}
		})
	}
}
