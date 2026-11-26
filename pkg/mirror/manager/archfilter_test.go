package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
)

func TestOwnerArchitectures(t *testing.T) {
	byIS := map[string][]string{
		"narrow": {"arm64", "amd64"},
		"other":  {"s390x", "amd64"},
		"all":    nil,
	}
	tests := []struct {
		owners []string
		want   []string
	}{
		{[]string{"narrow"}, []string{"amd64", "arm64"}},
		{[]string{"narrow", "other"}, []string{"amd64", "arm64", "s390x"}},
		{[]string{"narrow", "all"}, nil},
		{[]string{"unknown"}, nil},
		{[]string{"unknown", "narrow"}, []string{"amd64", "arm64"}},
		{nil, nil},
	}
	for _, tt := range tests {
		if got := ownerArchitectures(tt.owners, byIS); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ownerArchitectures(%v) = %v, want %v", tt.owners, got, tt.want)
		}
	}
}

func TestAnyOwnerEnabled(t *testing.T) {
	byIS := map[string]bool{"a": false, "b": true}
	if anyOwnerEnabled([]string{"a"}, byIS) || !anyOwnerEnabled([]string{"a", "b"}, byIS) || anyOwnerEnabled(nil, byIS) {
		t.Error("anyOwnerEnabled mismatch")
	}
}

// The ImageSet's architecture settings must reach the worker batch, per
// image and merged across owning ImageSets.
func TestDispatch_CarriesArchitectureFilter(t *testing.T) {
	m, cs := newReconcileTestManager(t, []string{"narrow", "plain"}, "narrow", "plain")
	ctx := context.Background()
	is := &mirrorv1alpha1.ImageSet{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: "narrow", Namespace: "default"}, is); err != nil {
		t.Fatal(err)
	}
	is.Spec.Mirror.Architectures = []string{"amd64", "arm64"}
	is.Spec.Mirror.SkipMissingArchitectures = true
	if err := m.Client.Update(ctx, is); err != nil {
		t.Fatal(err)
	}
	// Keep the ImageSet "resolved" so reconcile does not re-collect it.
	is.Status.ObservedGeneration = is.Generation
	if err := m.Client.Status().Update(ctx, is); err != nil {
		t.Fatal(err)
	}

	m.imageState = imagestate.ImageState{
		"reg.io/only-narrow:v1": {Source: "src/only-narrow:v1", State: statePending},
		"reg.io/shared:v1":      {Source: "src/shared:v1", State: statePending},
		"reg.io/only-plain:v1":  {Source: "src/only-plain:v1", State: statePending},
	}
	m.owners = map[string][]string{
		"reg.io/only-narrow:v1": {"narrow"},
		"reg.io/shared:v1":      {"narrow", "plain"},
		"reg.io/only-plain:v1":  {"plain"},
	}
	if err := m.reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	pods, err := cs.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]BatchItem{}
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				if e.Name != "MIRROR_BATCH" {
					continue
				}
				var items []BatchItem
				if err := json.Unmarshal([]byte(e.Value), &items); err != nil {
					t.Fatal(err)
				}
				for _, it := range items {
					got[it.Dest] = it
				}
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("dispatched %d items, want 3: %+v", len(got), got)
	}
	if it := got["reg.io/only-narrow:v1"]; !it.SkipMissingArchitectures || !reflect.DeepEqual(it.Architectures, []string{"amd64", "arm64"}) {
		t.Errorf("only-narrow = %+v", it)
	}
	// Shared with an ImageSet that wants every architecture: no list, but
	// skipping missing children is still allowed by "narrow".
	if it := got["reg.io/shared:v1"]; !it.SkipMissingArchitectures || it.Architectures != nil {
		t.Errorf("shared = %+v", it)
	}
	if it := got["reg.io/only-plain:v1"]; it.SkipMissingArchitectures || it.Architectures != nil {
		t.Errorf("only-plain = %+v", it)
	}
}

func TestArchReconcileNeeded(t *testing.T) {
	a := func(s ...string) []string { return s }
	tests := []struct {
		name              string
		recorded, want    []string
		prune             bool
		retain            []string
		backfill, doPrune bool
	}{
		{"unchanged all", nil, nil, true, nil, false, false},
		{"unchanged list", a("amd64"), a("amd64"), true, a("amd64"), false, false},
		{"widened list", a("amd64"), a("amd64", "arm64"), true, a("amd64", "arm64"), true, false},
		{"widened to all", a("amd64"), nil, true, nil, true, false},
		{"narrowed from all", nil, a("amd64"), true, a("amd64"), false, true},
		{"narrowed without cleanup policy", nil, a("amd64"), false, a("amd64"), false, false},
		{"narrowed but repo needs all", nil, a("amd64"), true, nil, false, false},
		{"swapped", a("amd64"), a("arm64"), true, a("arm64"), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, p := archReconcileNeeded(tt.recorded, tt.want, tt.prune, tt.retain)
			if b != tt.backfill || p != tt.doPrune {
				t.Errorf("got backfill=%v prune=%v, want %v %v", b, p, tt.backfill, tt.doPrune)
			}
		})
	}
}

func TestArchSelections_RetainPerRepository(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{
		"reg.io/a/img:v1": {State: stateMirrored},
		"reg.io/a/img@sha256:" + strings.Repeat("1", 64): {State: stateMirrored},
		"reg.io/b/img:v1":  {State: stateMirrored},
		"reg.io/c/img:v1":  {State: stateMirrored},
		"reg.io/orphan:v1": {State: stateMirrored},
	}
	m.owners = map[string][]string{
		"reg.io/a/img:v1": {"narrow"},
		"reg.io/a/img@sha256:" + strings.Repeat("1", 64): {"other"},
		"reg.io/b/img:v1": {"narrow", "all"},
		"reg.io/c/img:v1": {"narrow"},
	}
	archByIS := map[string][]string{"narrow": {"amd64"}, "other": {"arm64"}, "all": nil}
	want, retain := m.archSelectionsLocked(archByIS)
	if !reflect.DeepEqual(retain["reg.io/a/img"], []string{"amd64", "arm64"}) {
		t.Errorf("repo a retain = %v", retain["reg.io/a/img"])
	}
	if retain["reg.io/b/img"] != nil {
		t.Errorf("repo b retain = %v, want nil (one image needs all)", retain["reg.io/b/img"])
	}
	if !reflect.DeepEqual(want["reg.io/c/img:v1"], []string{"amd64"}) || want["reg.io/b/img:v1"] != nil {
		t.Errorf("want = %v", want)
	}
	if _, ok := want["reg.io/orphan:v1"]; ok {
		t.Error("entries without owners are skipped")
	}
	if repoOf("Bad Ref!") != "Bad Ref!" {
		t.Error("repoOf must fall back to the input")
	}
	if (*archUnion)(nil).list() != nil {
		t.Error("nil union lists nil")
	}
}

// reconcileArchTest builds a manager whose ImageSet "is" selects archs, with
// one Mirrored image recorded with recorded.
func reconcileArchTest(t *testing.T, archs, recorded []string, cleanupDelete bool) (*MirrorManager, *k8sfake.Clientset, string) {
	t.Helper()
	m, cs := newReconcileTestManager(t, []string{"is"}, "is")
	ctx := context.Background()
	is := &mirrorv1alpha1.ImageSet{}
	if err := m.Client.Get(ctx, client.ObjectKey{Name: "is", Namespace: "default"}, is); err != nil {
		t.Fatal(err)
	}
	is.Spec.Mirror.Architectures = archs
	if err := m.Client.Update(ctx, is); err != nil {
		t.Fatal(err)
	}
	is.Status.ObservedGeneration = is.Generation
	if err := m.Client.Status().Update(ctx, is); err != nil {
		t.Fatal(err)
	}
	if cleanupDelete {
		mt := &mirrorv1alpha1.MirrorTarget{}
		if err := m.Client.Get(ctx, client.ObjectKey{Name: "test", Namespace: "default"}, mt); err != nil {
			t.Fatal(err)
		}
		mt.Annotations = map[string]string{mirrorv1alpha1.CleanupPolicyAnnotation: mirrorv1alpha1.CleanupPolicyDelete}
		if err := m.Client.Update(ctx, mt); err != nil {
			t.Fatal(err)
		}
	}
	dest := "reg.io/confluentinc/cp-kafka-rest@sha256:" + strings.Repeat("c", 64)
	m.imageState = imagestate.ImageState{dest: {Source: "registry.connect.redhat.com/confluentinc/cp-kafka-rest@sha256:" + strings.Repeat("c", 64), State: stateMirrored, Architectures: recorded}}
	m.owners = map[string][]string{dest: {"is"}}
	return m, cs, dest
}

func dispatchedItems(t *testing.T, cs *k8sfake.Clientset) map[string]BatchItem {
	t.Helper()
	pods, err := cs.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]BatchItem{}
	for _, p := range pods.Items {
		for _, c := range p.Spec.Containers {
			for _, e := range c.Env {
				if e.Name != "MIRROR_BATCH" {
					continue
				}
				var items []BatchItem
				if err := json.Unmarshal([]byte(e.Value), &items); err != nil {
					t.Fatal(err)
				}
				for _, it := range items {
					got[it.Dest] = it
				}
			}
		}
	}
	return got
}

func TestReconcile_ArchitectureReconcileDispatch(t *testing.T) {
	tests := []struct {
		name          string
		archs         []string
		recorded      []string
		cleanupDelete bool
		wantDispatch  bool
		wantBackfill  bool
		wantPrune     []string
	}{
		{name: "widened", archs: []string{"amd64", "arm64"}, recorded: []string{"amd64"}, wantDispatch: true, wantBackfill: true},
		{name: "list removed", archs: nil, recorded: []string{"amd64"}, wantDispatch: true, wantBackfill: true},
		{name: "narrowed with cleanup policy", archs: []string{"amd64"}, recorded: nil, cleanupDelete: true, wantDispatch: true, wantPrune: []string{"amd64"}},
		{name: "narrowed without cleanup policy", archs: []string{"amd64"}, recorded: nil},
		{name: "unchanged", archs: []string{"amd64"}, recorded: []string{"amd64"}, cleanupDelete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, cs, dest := reconcileArchTest(t, tt.archs, tt.recorded, tt.cleanupDelete)
			if err := m.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			items := dispatchedItems(t, cs)
			it, ok := items[dest]
			if ok != tt.wantDispatch {
				t.Fatalf("dispatched = %v, want %v (%+v)", ok, tt.wantDispatch, items)
			}
			if ok && (it.Backfill != tt.wantBackfill || !reflect.DeepEqual(it.PruneRetain, tt.wantPrune) || !reflect.DeepEqual(it.Architectures, tt.archs)) {
				t.Errorf("item = %+v", it)
			}
			// The image stays Mirrored and in the IDMS while reconciling.
			if got := m.imageState[dest].State; got != stateMirrored {
				t.Errorf("state = %q, want Mirrored", got)
			}
			idms, err := resources.GenerateIDMS("test", m.imageState)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(idms), "reg.io/confluentinc/cp-kafka-rest") {
				t.Errorf("IDMS lost the mirror mapping while reconciling:\n%s", idms)
			}
		})
	}
}

func TestApplyStatus_ArchitectureReconcile(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	dest := "reg/img:v1"
	m.imageState = imagestate.ImageState{dest: {Source: "s", State: stateMirrored, RetryCount: 1}}
	m.owners = map[string][]string{dest: {"is"}}

	// A failed reconcile backs off and leaves the image Mirrored.
	m.inProgress[dest] = "pod"
	m.applyStatusLocked(WorkerStatusRequest{Destination: dest, Reconcile: true, Error: "DELETE not allowed", Architectures: []string{"amd64"}})
	e := m.imageState[dest]
	if e.State != stateMirrored || e.LastError != "" || e.RetryCount != 1 {
		t.Errorf("failed reconcile touched the mirror state: %+v", e)
	}
	if e.ArchRetryCount != 1 || e.ArchNextRetryAt == nil || e.Architectures != nil {
		t.Errorf("expected backoff without recording: %+v", e)
	}
	if _, ok := m.inProgress[dest]; ok {
		t.Error("expected inProgress to be cleared")
	}
	// While backing off, no reconcile is dispatched.
	if _, ok := m.archReconcileItemLocked(dest, e, []string{"amd64"}, false, true, map[string][]string{"reg/img": {"amd64"}}, time.Now()); ok {
		t.Error("expected no reconcile during backoff")
	}

	// A successful reconcile records the selection and clears the backoff.
	m.applyStatusLocked(WorkerStatusRequest{Destination: dest, Reconcile: true, Digest: "sha256:x", Architectures: []string{"arm64", "amd64"}})
	if !reflect.DeepEqual(e.Architectures, []string{"amd64", "arm64"}) || e.ArchRetryCount != 0 || e.ArchNextRetryAt != nil {
		t.Errorf("after success: %+v", e)
	}
	// Unknown destinations are ignored.
	m.applyStatusLocked(WorkerStatusRequest{Destination: "gone", Reconcile: true})
}

func TestApplyStatus_RecordsArchitecturesOnFirstMirror(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	dest := "reg/img:v1"
	m.imageState = imagestate.ImageState{dest: {Source: "s", State: statePending}}
	m.owners = map[string][]string{dest: {"is"}}
	m.applyStatusLocked(WorkerStatusRequest{Destination: dest, Digest: "sha256:x", Architectures: []string{"amd64"}})
	if e := m.imageState[dest]; e.State != stateMirrored || !reflect.DeepEqual(e.Architectures, []string{"amd64"}) {
		t.Errorf("entry = %+v", e)
	}
}

func TestShouldMirror_Reconcile(t *testing.T) {
	m := newTestManager("tok", imagestate.ImageState{
		"reg/m": {State: stateMirrored},
		"reg/p": {State: statePending},
	})
	for dest, want := range map[string]int{"reg/m": http.StatusOK, "reg/p": http.StatusGone, "reg/x": http.StatusGone} {
		req := httptest.NewRequest(http.MethodGet, "/should-mirror?reconcile=1&dest="+dest, nil)
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		m.handleShouldMirror(rr, req)
		if rr.Code != want {
			t.Errorf("%s: code = %d, want %d", dest, rr.Code, want)
		}
	}
}

// A worker pod that dies during a reconcile must not reset the image to
// Pending (which would withdraw it from the IDMS).
func TestCleanupFinishedWorkers_FailedReconcilePodKeepsMirrored(t *testing.T) {
	labels := map[string]string{"app": "oc-mirror-worker", "mirrortarget": "t"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "default", Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	cs := k8sfake.NewSimpleClientset(pod)
	m := NewWithClients(nil, cs, "t", "default", "img", "", runtime.NewScheme())
	m.imageState = imagestate.ImageState{
		"mirrored": {Source: "s1", State: stateMirrored},
		"pending":  {Source: "s2", State: statePending},
	}
	m.owners = map[string][]string{"mirrored": {"is"}, "pending": {"is"}}
	m.inProgress = map[string]string{"mirrored": "w", "pending": "w"}

	m.cleanupFinishedWorkers(context.Background())

	if e := m.imageState["mirrored"]; e.State != stateMirrored || e.ArchRetryCount != 1 || e.ArchNextRetryAt == nil {
		t.Errorf("reconciled entry = %+v", e)
	}
	if got := m.imageState["pending"].State; got != statePending {
		t.Errorf("pending entry state = %q", got)
	}
}

// The recorded architectures and the reconcile backoff of a mirrored image
// must survive a re-resolve; otherwise a widened selection would never be
// backfilled and a narrowed one pruned again on every resolve.
func TestArchState_SurvivesReResolve(t *testing.T) {
	next := metav1.NewTime(time.Now().Add(time.Hour))
	mirrored := func() *imagestate.ImageEntry {
		return &imagestate.ImageEntry{Source: "quay.io/img:v1", State: stateMirrored, Origin: imagestate.OriginOperator,
			Architectures: []string{"amd64"}, ArchRetryCount: 2, ArchNextRetryAt: &next}
	}
	check := func(t *testing.T, e *imagestate.ImageEntry) {
		t.Helper()
		if e.State != stateMirrored || !reflect.DeepEqual(e.Architectures, []string{"amd64"}) || e.ArchRetryCount != 2 || e.ArchNextRetryAt == nil {
			t.Errorf("entry = %+v", e)
		}
	}

	t.Run("mergeIntoStateWithSig", func(t *testing.T) {
		dst := imagestate.ImageState{}
		prev := imagestate.ImageState{"reg.io/img:v1": mirrored()}
		mergeIntoStateWithSig(dst, []mirror.TargetImage{{Source: "quay.io/img:v1", Destination: "reg.io/img:v1"}},
			imagestate.OriginOperator, "sig", "cat", prev)
		check(t, dst["reg.io/img:v1"])
		prev["reg.io/img:v1"].Architectures[0] = "changed"
		if dst["reg.io/img:v1"].Architectures[0] != "amd64" {
			t.Error("architectures must be copied, not shared")
		}
	})
	t.Run("mergeWorkerUpdates", func(t *testing.T) {
		resolved := imagestate.ImageState{"reg.io/img:v1": {Source: "quay.io/img:v1", State: statePending}}
		live := imagestate.ImageState{"reg.io/img:v1": mirrored()}
		check(t, mergeWorkerUpdates(resolved, live)["reg.io/img:v1"])
	})
}

func TestArchReconcile_SkipsHelmCharts(t *testing.T) {
	m := NewWithClients(nil, nil, "t", "default", "img", "", runtime.NewScheme())
	chart := mirror.HelmChartSource("https://charts.example.com", "app", "1.0.0")
	dest := "reg.io/charts/app:1.0.0"
	m.imageState = imagestate.ImageState{dest: {Source: chart, State: stateMirrored}}
	m.owners = map[string][]string{dest: {"narrow"}}
	want, retain := m.archSelectionsLocked(map[string][]string{"narrow": {"amd64"}})
	if _, ok := want[dest]; ok || len(retain) != 0 {
		t.Errorf("chart must not get a selection: want=%v retain=%v", want, retain)
	}
	if _, ok := m.archReconcileItemLocked(dest, m.imageState[dest], []string{"amd64"}, false, true,
		map[string][]string{"reg.io/charts/app": {"amd64"}}, time.Now()); ok {
		t.Error("chart must not be reconciled")
	}
}

// Review #299 P1: a queued prune is confirmed against the current policy and
// retain set right before the worker deletes anything.
func TestHandleConfirmPrune(t *testing.T) {
	const dest = "reg.io/ns/app:v1"
	tests := []struct {
		name    string
		prune   bool
		state   string
		current []string
		known   bool
		retain  string
		want    int
	}{
		{"plan still valid", true, stateMirrored, []string{"amd64"}, true, "amd64", http.StatusOK},
		{"plan keeps more than needed", true, stateMirrored, []string{"amd64"}, true, "amd64,arm64", http.StatusOK},
		{"cleanup policy removed", false, stateMirrored, []string{"amd64"}, true, "amd64", http.StatusGone},
		{"repository now needs arm64", true, stateMirrored, []string{"amd64", "arm64"}, true, "amd64", http.StatusGone},
		{"repository now needs all", true, stateMirrored, nil, true, "amd64", http.StatusGone},
		{"repository unknown", true, stateMirrored, nil, false, "amd64", http.StatusGone},
		{"image no longer mirrored", true, statePending, []string{"amd64"}, true, "amd64", http.StatusGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager("tok", imagestate.ImageState{dest: {State: tt.state}})
			m.pruneArchitectures = tt.prune
			m.retainArchByRepo = map[string][]string{}
			if tt.known {
				m.retainArchByRepo["reg.io/ns/app"] = tt.current
			}
			if got := doConfirmPrune(m, dest, tt.retain); got != tt.want {
				t.Errorf("code = %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("fails closed while the state is locked", func(t *testing.T) {
		m := newTestManager("tok", imagestate.ImageState{dest: {State: stateMirrored}})
		m.pruneArchitectures = true
		m.retainArchByRepo = map[string][]string{"reg.io/ns/app": {"amd64"}}
		m.mu.Lock()
		defer m.mu.Unlock()
		if got := doConfirmPrune(m, dest, "amd64"); got != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", got)
		}
	})
}

func doConfirmPrune(m *MirrorManager, dest, retain string) int {
	req := httptest.NewRequest(http.MethodGet, "/should-mirror?reconcile=1&dest="+dest+"&prune="+retain, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	m.handleShouldMirror(rr, req)
	return rr.Code
}

// The tick publishes the prune policy and retain sets the confirmation uses.
func TestReconcile_PublishesPrunePlanState(t *testing.T) {
	m, _, dest := reconcileArchTest(t, []string{"amd64"}, nil, true)
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.pruneArchitectures || !reflect.DeepEqual(m.retainArchByRepo[repoOf(dest)], []string{"amd64"}) {
		t.Errorf("prune=%v retain=%v", m.pruneArchitectures, m.retainArchByRepo)
	}
}
