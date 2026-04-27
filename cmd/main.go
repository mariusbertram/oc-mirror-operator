package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/internal/controller"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/manager"
	"github.com/mariusbertram/oc-mirror-operator/pkg/resourceapi/ui"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mirrorv1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "manager":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runManager()
			return
		case "worker":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runWorker()
			return
		case "cleanup":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runCleanup()
			return
		case "resource-api":
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))
			runResourceAPI()
			return
		}
	}
	runController()
}

func runResourceAPI() {
	var namespace string
	fsApi := flag.NewFlagSet("resource-api", flag.ExitOnError)
	fsApi.StringVar(&namespace, "namespace", "", "Namespace to watch")
	_ = fsApi.Parse(os.Args[2:])

	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	cfg := ctrl.GetConfigOrDie()
	c, _ := client.New(cfg, client.Options{Scheme: scheme})
	api := &ResourceAPI{client: c, namespace: namespace}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/targets", api.handleTargets)
	mux.HandleFunc("/api/v1/targets/{target}", api.handleTargetDetail)
	mux.HandleFunc("/api/v1/targets/{target}/imagesets/{imageset}/", api.handleResource)

	staticFs, _ := fs.Sub(ui.StaticAssets, ".")
	mux.Handle("/", http.FileServer(http.FS(staticFs)))

	server := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("Resource API listening on :8081 in namespace %s\n", namespace)
	_ = server.ListenAndServe()
}

type ResourceAPI struct {
	client    client.Client
	namespace string
}

func (a *ResourceAPI) handleTargets(w http.ResponseWriter, r *http.Request) {
	var mtList mirrorv1alpha1.MirrorTargetList
	if err := a.client.List(r.Context(), &mtList, client.InNamespace(a.namespace)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mtList.Items)
}

func (a *ResourceAPI) handleTargetDetail(w http.ResponseWriter, r *http.Request) {
	targetName := r.PathValue("target")
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := a.client.Get(r.Context(), client.ObjectKey{Namespace: a.namespace, Name: targetName}, mt); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cm := &corev1.ConfigMap{}
	cmName := fmt.Sprintf("%s-resource-index", targetName)
	if err := a.client.Get(r.Context(), client.ObjectKey{Namespace: a.namespace, Name: cmName}, cm); err == nil {
		var index interface{}
		if err := json.Unmarshal([]byte(cm.Data["data"]), &index); err == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"target": mt, "index": index})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mt)
}

func (a *ResourceAPI) handleResource(w http.ResponseWriter, r *http.Request) {
	targetName := r.PathValue("target")
	isName := r.PathValue("imageset")
	path := r.URL.Path
	idx := strings.Index(path, isName)
	if idx == -1 {
		http.NotFound(w, r)
		return
	}
	suffix := path[idx+len(isName)+1:]
	resType := strings.ReplaceAll(strings.TrimSuffix(suffix, ".yaml"), "/", "-")
	resType = strings.ReplaceAll(resType, ".json", "")

	cmName := fmt.Sprintf("%s-resource-%s-%s", targetName, isName, resType)
	cm := &corev1.ConfigMap{}
	if err := a.client.Get(r.Context(), client.ObjectKey{Namespace: a.namespace, Name: cmName}, cm); err != nil {
		http.Error(w, "Resource not found", http.StatusNotFound)
		return
	}
	if strings.HasSuffix(path, ".yaml") {
		w.Header().Set("Content-Type", "text/yaml")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	_, _ = w.Write([]byte(cm.Data["data"]))
}

func runManager() {
	var targetName, namespace string
	fsMan := flag.NewFlagSet("manager", flag.ExitOnError)
	fsMan.StringVar(&targetName, "mirrortarget", "", "Name of the MirrorTarget")
	fsMan.StringVar(&namespace, "namespace", "", "Namespace")
	_ = fsMan.Parse(os.Args[2:])
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	m, _ := manager.New(targetName, namespace, scheme)
	_ = m.Run(ctrl.SetupSignalHandler())
}

func runWorker() {
	var insecure bool
	fsWork := flag.NewFlagSet("worker", flag.ExitOnError)
	fsWork.BoolVar(&insecure, "insecure", false, "Insecure registry")
	_ = fsWork.Parse(os.Args[2:])
	if batchJSON := os.Getenv("MIRROR_BATCH"); batchJSON != "" {
		runWorkerBatch(insecure, batchJSON)
	}
}

func runWorkerBatch(insecure bool, batchJSON string) {
	var items []BatchItem
	_ = json.Unmarshal([]byte(batchJSON), &items)
	if len(items) == 0 {
		return
	}
	c := buildMirrorClient(insecure, items[0].Dest)
	sources, dests := make([]string, len(items)), make([]string, len(items))
	for i, item := range items {
		sources[i], dests[i] = item.Source, item.Dest
	}
	sources, dests = mirror.PlanMirrorOrder(context.Background(), c, sources, dests)
	for i := range sources {
		if !shouldMirror(dests[i]) {
			continue
		}
		if i > 0 && i%20 == 0 {
			c = buildMirrorClient(insecure, dests[i])
		}
		mirrorOneImage(c, sources[i], dests[i])
	}
}

type BatchItem struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

func buildMirrorClient(insecure bool, firstDest string) *mirrorclient.MirrorClient {
	destHost := ""
	if parts := strings.Split(firstDest, "/"); len(parts) > 0 {
		destHost = parts[0]
	}
	var insecureHosts []string
	if insecure && destHost != "" {
		insecureHosts = append(insecureHosts, destHost)
	}
	return mirrorclient.NewMirrorClient(insecureHosts, os.Getenv("DOCKER_CONFIG"), destHost)
}

func runCleanup() {
	var namespace, registry, snapshotName string
	var insecure bool
	fsClean := flag.NewFlagSet("cleanup", flag.ExitOnError)
	fsClean.StringVar(&namespace, "namespace", "", "Namespace")
	fsClean.StringVar(&registry, "registry", "", "Registry")
	fsClean.BoolVar(&insecure, "insecure", false, "Insecure")
	fsClean.StringVar(&snapshotName, "snapshot", "", "Snapshot CM")
	_ = fsClean.Parse(os.Args[2:])
	c, _ := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	state, _ := imagestate.LoadByConfigMapName(context.Background(), c, namespace, snapshotName)
	mc := buildMirrorClient(insecure, registry)
	for dest, entry := range state {
		if entry.State == "Mirrored" {
			_ = mc.DeleteManifest(context.Background(), dest)
		}
	}
	_ = c.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: snapshotName, Namespace: namespace}})
}

func shouldMirror(dest string) bool {
	managerURL := os.Getenv("MANAGER_URL")
	if managerURL == "" {
		return true
	}
	resp, err := http.Get(managerURL + "/should-mirror?dest=" + url.QueryEscape(dest))
	if err != nil {
		return true
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusGone
}

func mirrorOneImage(c *mirrorclient.MirrorClient, src, dest string) bool {
	_, err := c.CopyImage(context.Background(), src, dest)
	if err != nil {
		reportStatus(dest, "", err.Error())
		return false
	}
	digest, _ := c.GetDigest(context.Background(), dest)
	reportStatus(dest, digest, "")
	return true
}

func reportStatus(dest, digest, errMsg string) {
	managerURL, podName := os.Getenv("MANAGER_URL"), os.Getenv("POD_NAME")
	if managerURL == "" || podName == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"podName": podName, "destination": dest, "digest": digest, "error": errMsg})
	resp, err := http.Post(managerURL+"/status", "application/json", bytes.NewBuffer(body))
	if err == nil {
		_ = resp.Body.Close()
	}
}

func runController() {
	mgr, _ := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme})
	_ = (&controller.MirrorTargetReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)
	_ = mgr.Start(ctrl.SetupSignalHandler())
}
