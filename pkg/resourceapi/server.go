package resourceapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

func (s *Server) Run(ctx context.Context) {
	r := mux.NewRouter()

	// Redirect handler for legacy /resources/{imageset}/... paths
	r.PathPrefix("/resources/{imageset}/").HandlerFunc(s.handleLegacyRedirect)

	// New API structure
	// /api/v1/targets/{mt}/resources/idms
	// /api/v1/targets/{mt}/resources/itms
	// /api/v1/targets/{mt}/resources/catalogs/{slug}/catalogsource
	// /api/v1/targets/{mt}/resources/catalogs/{slug}/packages

	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/targets/{mt}/resources/idms", s.handleIDMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/resources/itms", s.handleITMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/resources/catalogs/{slug}/catalogsource", s.handleCatalogSource).Methods("GET")
	api.HandleFunc("/targets/{mt}/resources/catalogs/{slug}/packages", s.handleCatalogPackages).Methods("GET")

	srv := &http.Server{
		Addr:    ":8081",
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Resource API listen error: %v\n", err)
		}
	}()

	fmt.Println("Resource API started on :8081")
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}

func (s *Server) handleLegacyRedirect(w http.ResponseWriter, r *http.Request) {
	// Simple redirect for now - in production we would resolve the IS to its MT
	w.WriteHeader(http.StatusGone)
	_, _ = fmt.Fprintln(w, "Legacy /resources/ paths are no longer supported. Please use the new /api/v1/targets/{mt}/... API.")
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
	slug := vars["slug"]
	s.serveConfigMapResource(w, r, fmt.Sprintf("oc-mirror-%s-packages", slug), "packages.json")
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
