package manager

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
)

// Worker pods only reference a pull secret when one is configured (#151).
func TestStartWorkerBatch_ImagePullSecrets(t *testing.T) {
	for name, authSecret := range map[string]string{"no auth secret": "", "auth secret": "pull-secret"} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = mirrorv1alpha1.AddToScheme(scheme)
			cs := k8sfake.NewSimpleClientset()
			m := NewWithClients(nil, cs, "mt", "default", "img", "", scheme)
			mt := &mirrorv1alpha1.MirrorTarget{
				ObjectMeta: metav1.ObjectMeta{Name: "mt", Namespace: "default", UID: "uid"},
				Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io", AuthSecret: authSecret},
			}
			if _, err := m.startWorkerBatch(context.Background(), mt, []BatchItem{{Source: "s", Dest: "d"}}); err != nil {
				t.Fatal(err)
			}
			pods, err := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
			if err != nil || len(pods.Items) != 1 {
				t.Fatalf("pods: %v, err %v", pods, err)
			}
			got := pods.Items[0].Spec.ImagePullSecrets
			switch {
			case authSecret == "" && len(got) != 0:
				t.Errorf("imagePullSecrets = %v, want none", got)
			case authSecret != "" && (len(got) != 1 || got[0].Name != authSecret):
				t.Errorf("imagePullSecrets = %v, want [%s]", got, authSecret)
			}
		})
	}
}
