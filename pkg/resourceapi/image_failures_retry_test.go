package resourceapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// The image failures endpoint reports when a failed image in backoff is
// retried next (#144).
func TestImageFailures_ReportsNextRetry(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	_ = mirrorv1alpha1.AddToScheme(sc)
	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt", Namespace: "ns"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io", ImageSets: []string{"is"}},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(mt).Build()
	next := metav1.NewTime(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC))
	state := imagestate.ImageState{
		"reg.io/backoff:v1": {Source: "s1", State: "Failed", RetryCount: 2, NextRetryAt: &next},
		"reg.io/pending:v1": {Source: "s2", State: "Pending"},
	}
	if err := imagestate.Save(context.Background(), c, "ns", "is", state, nil, nil); err != nil {
		t.Fatal(err)
	}

	r := mux.NewRouter()
	NewServerForTest(c, "ns").RegisterAPIRoutes(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/targets/mt/image-failures", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp ImageFailuresResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range resp.Pending {
		got[d.Destination] = d.NextRetryAt
	}
	if got["reg.io/backoff:v1"] != "2030-01-02T03:04:05Z" {
		t.Errorf("nextRetryAt = %q, want 2030-01-02T03:04:05Z", got["reg.io/backoff:v1"])
	}
	if got["reg.io/pending:v1"] != "" {
		t.Errorf("a pending image has no next retry, got %q", got["reg.io/pending:v1"])
	}
}
