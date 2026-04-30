package resourceapi

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed ui
var uiFS embed.FS

type Server struct {
	client    client.Client
	namespace string
}

func NewServer(c client.Client, namespace string) *Server {
	return &Server{
		client:    c,
		namespace: namespace,
	}
}

// TargetSummary is the JSON response for the targets list endpoint.
type TargetSummary struct {
	Name           string `json:"name"`
	Registry       string `json:"registry"`
	TotalImages    int    `json:"totalImages"`
	MirroredImages int    `json:"mirroredImages"`
	PendingImages  int    `json:"pendingImages"`
	FailedImages   int    `json:"failedImages"`
}

// TargetDetail is the JSON response for a single target detail endpoint.
type TargetDetail struct {
	Name           string                `json:"name"`
	Registry       string                `json:"registry"`
	TotalImages    int                   `json:"totalImages"`
	MirroredImages int                   `json:"mirroredImages"`
	PendingImages  int                   `json:"pendingImages"`
	FailedImages   int                   `json:"failedImages"`
	ImageSets      []ImageSetSummaryJSON `json:"imageSets"`
	Resources      []ResourceLink        `json:"resources"`
}

// ImageSetSummaryJSON is the per-ImageSet status info returned in JSON.
type ImageSetSummaryJSON struct {
	Name      string         `json:"name"`
	Found     bool           `json:"found"`
	Total     int            `json:"total"`
	Mirrored  int            `json:"mirrored"`
	Pending   int            `json:"pending"`
	Failed    int            `json:"failed"`
	Resources []ResourceLink `json:"resources"`
}

// ResourceLink describes a downloadable resource.
type ResourceLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

func (s *Server) RegisterRoutes(r *mux.Router) {
	// Serve embedded Web UI at root
	uiSub, err := fs.Sub(uiFS, "ui")
	if err == nil {
		r.PathPrefix("/ui/").Handler(http.StripPrefix("/ui/", http.FileServer(http.FS(uiSub))))
		r.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/ui/", http.StatusMovedPermanently)
		})
	}

	// Legacy redirect
	r.PathPrefix("/resources/{imageset}/").HandlerFunc(s.handleLegacyRedirect)

	// API endpoints – JSON metadata
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/targets", s.handleTargetsList).Methods("GET")
	api.HandleFunc("/targets/{mt}", s.handleTargetDetail).Methods("GET")

	// API endpoints – raw resources from ConfigMaps
	api.HandleFunc("/targets/{mt}/imagesets/{is}/idms.yaml", s.handleIDMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/itms.yaml", s.handleITMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/catalogsource.yaml", s.handleCatalogSource).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/upstream-packages.json", s.handleCatalogPackages).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/packages.json", s.handleCatalogPackages).Methods("GET")
}

func (s *Server) Run(ctx context.Context) {
	r := mux.NewRouter()
	s.RegisterRoutes(r)

	srv := &http.Server{
		Addr:              ":8081",
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Resource API listen error: %v\n", err)
		}
	}()

	fmt.Println("Resource API started on :8081")
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// --- JSON API handlers ---

func (s *Server) handleTargetsList(w http.ResponseWriter, r *http.Request) {
	list := &mirrorv1alpha1.MirrorTargetList{}
	if err := s.client.List(r.Context(), list, client.InNamespace(s.namespace)); err != nil {
		http.Error(w, fmt.Sprintf("failed to list MirrorTargets: %v", err), http.StatusInternalServerError)
		return
	}

	targets := make([]TargetSummary, 0, len(list.Items))
	for _, mt := range list.Items {
		targets = append(targets, TargetSummary{
			Name:           mt.Name,
			Registry:       mt.Spec.Registry,
			TotalImages:    mt.Status.TotalImages,
			MirroredImages: mt.Status.MirroredImages,
			PendingImages:  mt.Status.PendingImages,
			FailedImages:   mt.Status.FailedImages,
		})
	}

	writeJSON(w, targets)
}

func (s *Server) handleTargetDetail(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]

	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := s.client.Get(r.Context(), client.ObjectKey{Name: mtName, Namespace: s.namespace}, mt); err != nil {
		http.Error(w, "MirrorTarget not found", http.StatusNotFound)
		return
	}

	// Build resource links from the resources ConfigMap
	cmName := fmt.Sprintf("oc-mirror-%s-resources", mtName)
	cm := &corev1.ConfigMap{}
	var resources []ResourceLink
	if err := s.client.Get(r.Context(), client.ObjectKey{Name: cmName, Namespace: s.namespace}, cm); err == nil {
		resources = buildResourceLinks(mtName, cm)
	}

	imageSets := make([]ImageSetSummaryJSON, 0, len(mt.Status.ImageSetStatuses))
	for _, iss := range mt.Status.ImageSetStatuses {
		imageSets = append(imageSets, ImageSetSummaryJSON{
			Name:      iss.Name,
			Found:     iss.Found,
			Total:     iss.Total,
			Mirrored:  iss.Mirrored,
			Pending:   iss.Pending,
			Failed:    iss.Failed,
			Resources: resources, // same resources apply to all image sets for now
		})
	}

	detail := TargetDetail{
		Name:           mt.Name,
		Registry:       mt.Spec.Registry,
		TotalImages:    mt.Status.TotalImages,
		MirroredImages: mt.Status.MirroredImages,
		PendingImages:  mt.Status.PendingImages,
		FailedImages:   mt.Status.FailedImages,
		ImageSets:      imageSets,
		Resources:      resources,
	}

	writeJSON(w, detail)
}

// --- Raw resource handlers ---

func (s *Server) handleLegacyRedirect(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusGone)
	_, _ = fmt.Fprintln(w, "Legacy /resources/ paths are deprecated. Please use the new /api/v1/targets/{mt}/imagesets/{is}/... API.")
}

func (s *Server) handleIDMS(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	s.serveConfigMapResource(w, r, fmt.Sprintf("oc-mirror-%s-resources", mtName), "idms.yaml")
}

func (s *Server) handleITMS(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	s.serveConfigMapResource(w, r, fmt.Sprintf("oc-mirror-%s-resources", mtName), "itms.yaml")
}

func (s *Server) handleCatalogSource(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	s.serveConfigMapResource(w, r, fmt.Sprintf("oc-mirror-%s-resources", mtName), fmt.Sprintf("catalogsource-%s.yaml", slug))
}

func (s *Server) handleCatalogPackages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	s.serveConfigMapResource(w, r, fmt.Sprintf("oc-mirror-%s-%s-packages", mtName, slug), "packages.json")
}

func (s *Server) serveConfigMapResource(w http.ResponseWriter, r *http.Request, cmName, key string) {
	cm := &corev1.ConfigMap{}
	err := s.client.Get(r.Context(), client.ObjectKey{Name: cmName, Namespace: s.namespace}, cm)
	if err != nil {
		http.Error(w, "Resource not found", http.StatusNotFound)
		return
	}

	data, ok := cm.Data[key]
	if !ok {
		http.Error(w, "Resource key not found in ConfigMap", http.StatusNotFound)
		return
	}

	if strings.HasSuffix(key, ".json") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/yaml")
	}
	_, _ = w.Write([]byte(data))
}

// --- helpers ---

func buildResourceLinks(mtName string, cm *corev1.ConfigMap) []ResourceLink {
	var links []ResourceLink
	base := fmt.Sprintf("/api/v1/targets/%s/imagesets/latest", mtName)

	if _, ok := cm.Data["idms.yaml"]; ok {
		links = append(links, ResourceLink{Name: "IDMS", URL: base + "/idms.yaml", Type: "yaml"})
	}
	if _, ok := cm.Data["itms.yaml"]; ok {
		links = append(links, ResourceLink{Name: "ITMS", URL: base + "/itms.yaml", Type: "yaml"})
	}

	// Detect catalog resources by key pattern catalogsource-<slug>.yaml
	for key := range cm.Data {
		if strings.HasPrefix(key, "catalogsource-") && strings.HasSuffix(key, ".yaml") {
			slug := strings.TrimSuffix(strings.TrimPrefix(key, "catalogsource-"), ".yaml")
			links = append(links, ResourceLink{
				Name: fmt.Sprintf("CatalogSource (%s)", slug),
				URL:  fmt.Sprintf("%s/catalogs/%s/catalogsource.yaml", base, slug),
				Type: "yaml",
			})
		}
	}

	return links
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}
