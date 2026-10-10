package resourceapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
)

// The progress endpoint aggregates counts across all ImageSets of a
// MirrorTarget and reports a percentage (#207).
func TestTargetProgress_CountsAndPercent(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	_ = mirrorv1alpha1.AddToScheme(sc)

	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt", Namespace: "ns"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io", ImageSets: []string{"is"}},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(mt).Build()

	state := imagestate.ImageState{
		"reg.io/ok:v1":      {Source: "s1", State: "Mirrored"},
		"reg.io/ok:v2":      {Source: "s2", State: "Mirrored"},
		"reg.io/pending:v1": {Source: "s3", State: "Pending"},
		"reg.io/backoff:v1": {Source: "s4", State: "Failed", RetryCount: 9, PermanentlyFailed: true},
	}
	if err := imagestate.Save(context.Background(), c, "ns", "is", state, nil, nil); err != nil {
		t.Fatal(err)
	}

	r := mux.NewRouter()
	NewServerForTest(c, "ns").RegisterAPIRoutes(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/targets/mt/progress", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp TargetProgress
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 || resp.Mirrored != 2 || resp.Pending != 1 || resp.Failed != 1 {
		t.Errorf("counts = total %d, mirrored %d, pending %d, failed %d; want 4/2/1/1",
			resp.Total, resp.Mirrored, resp.Pending, resp.Failed)
	}
	if resp.Percent != 50 {
		t.Errorf("percent = %v, want 50", resp.Percent)
	}
	if len(resp.ImageSets) != 1 || resp.ImageSets[0].Name != "is" {
		t.Errorf("imageSets = %+v, want one entry for is", resp.ImageSets)
	}
}

// Without any state the progress endpoint reports zeroes and no ETA rather
// than a division by zero.
func TestTargetProgress_EmptyStateIsSafe(t *testing.T) {
	sc := runtime.NewScheme()
	_ = corev1.AddToScheme(sc)
	_ = mirrorv1alpha1.AddToScheme(sc)

	mt := &mirrorv1alpha1.MirrorTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "mt", Namespace: "ns"},
		Spec:       mirrorv1alpha1.MirrorTargetSpec{Registry: "reg.io", ImageSets: []string{"is"}},
	}
	c := fake.NewClientBuilder().WithScheme(sc).WithObjects(mt).Build()

	r := mux.NewRouter()
	NewServerForTest(c, "ns").RegisterAPIRoutes(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/targets/mt/progress", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp TargetProgress
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 0 || resp.Percent != 0 || resp.EtaSeconds != nil {
		t.Errorf("empty state must yield zero counts and no eta, got %+v", resp)
	}
}
