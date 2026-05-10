package resourceapi

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	mirrorresources "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientgorest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

//go:embed plugin
var pluginFS embed.FS

type tokenClientEntry struct {
	c         client.Client
	expiresAt time.Time
}

type Server struct {
	client       client.Client
	namespace    string // empty = cluster-wide mode
	scheme       *runtime.Scheme
	baseCfg      *clientgorest.Config
	tokenClients sync.Map // sha256(token) -> *tokenClientEntry, TTL 5 min
	channelCache channelCacheState
}

// channelCacheState holds the in-memory cache for the OCP channel list.
type channelCacheState struct {
	mu        sync.Mutex
	entries   []ocpChannelEntry
	fetchedAt time.Time
}

// ocpChannelEntry describes a single OCP/OKD release channel returned by the
// /api/v1/releases/channels endpoint.
type ocpChannelEntry struct {
	Name    string `json:"name"`    // e.g. "stable-4.18"
	Type    string `json:"type"`    // "ocp" or "okd"
	Version string `json:"version"` // e.g. "4.18"
}

// githubContentEntry is a single item from the GitHub Contents API response.
type githubContentEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
}

const platformTypeOKD = "okd"

// openshift/cincinnati-graph-data channels directory.
// See: https://github.com/openshift/cincinnati-graph-data/tree/master/channels
const githubGraphDataChannelsURL = "https://api.github.com/repos/openshift/cincinnati-graph-data/contents/channels"

// channelCacheTTL controls how long the OCP channel list is cached before
// re-fetching from the upstream source.
const channelCacheTTL = time.Hour

// defaultOCPChannels is the built-in fallback list used when both the GitHub
// API and the ConfigMap override are unavailable (e.g. air-gapped clusters).
var defaultOCPChannels = func() []ocpChannelEntry {
	types := []string{"stable", "fast", "eus", "candidate"}
	versions := []string{"4.14", "4.15", "4.16", "4.17", "4.18", "4.19"}
	entries := make([]ocpChannelEntry, 0, len(versions)*len(types))
	for _, ver := range versions {
		minor, _ := strconv.Atoi(strings.Split(ver, ".")[1])
		for _, t := range types {
			if t == "eus" && minor%2 != 0 {
				continue
			}
			entries = append(entries, ocpChannelEntry{
				Name:    t + "-" + ver,
				Type:    "ocp",
				Version: ver,
			})
		}
	}
	return entries
}()

func NewServer(c client.Client, namespace string) *Server {
	cfg, _ := config.GetConfig()
	return &Server{
		client:    c,
		namespace: namespace,
		scheme:    c.Scheme(),
		baseCfg:   cfg,
	}
}

// NewServerClusterWide creates a Server that lists MirrorTargets across all namespaces.
// Used by the standalone dashboard binary.
func NewServerClusterWide(c client.Client) *Server {
	cfg, _ := config.GetConfig()
	return &Server{
		client:  c,
		scheme:  c.Scheme(),
		baseCfg: cfg,
	}
}

// LookupMirrorTarget fetches a MirrorTarget by name. In namespace-bound mode the
// stored namespace is used; in cluster-wide mode all namespaces are searched.
func (s *Server) LookupMirrorTarget(ctx context.Context, c client.Client, name string) (*mirrorv1alpha1.MirrorTarget, error) {
	if s.namespace != "" {
		mt := &mirrorv1alpha1.MirrorTarget{}
		return mt, c.Get(ctx, client.ObjectKey{Name: name, Namespace: s.namespace}, mt)
	}

	// Cluster-wide search: find by name in any namespace.
	list := &mirrorv1alpha1.MirrorTargetList{}
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(mirrorv1alpha1.GroupVersion.WithResource("mirrortargets").GroupResource(), name)
}

// clientForRequest builds a K8s client scoped to the caller's Bearer token.
// The token is read from X-Forwarded-Access-Token (set by oauth-proxy) or from
// the Authorization header (used by the Console Plugin SDK via consoleFetch).
// If neither is present, the server's own service-account client is returned.
//
// Clients are cached by token hash for 5 minutes to avoid creating a new HTTP
// connection pool on every request (the UI polls every 30 seconds).
func (s *Server) clientForRequest(r *http.Request) client.Client {
	token := r.Header.Get("X-Forwarded-Access-Token")
	if token == "" {
		auth := r.Header.Get("Authorization")
		token = strings.TrimPrefix(auth, "Bearer ")
	}
	if token == "" || s.baseCfg == nil {
		return s.client
	}

	h := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(h[:])

	now := time.Now()
	if v, ok := s.tokenClients.Load(key); ok {
		entry := v.(*tokenClientEntry)
		if entry.expiresAt.After(now) {
			return entry.c
		}
		s.tokenClients.Delete(key)
	}

	fmt.Printf("clientForRequest: creating new client for token (%d bytes)\n", len(token))
	cfg := clientgorest.CopyConfig(s.baseCfg)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	c, err := client.New(cfg, client.Options{Scheme: s.scheme})
	if err != nil {
		fmt.Printf("clientForRequest: failed to create client from token: %v\n", err)
		return s.client
	}

	s.tokenClients.Store(key, &tokenClientEntry{c: c, expiresAt: now.Add(5 * time.Minute)})
	return c
}

// TargetSummary is the JSON response for the targets list endpoint.
type TargetSummary struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Registry       string `json:"registry"`
	TotalImages    int    `json:"totalImages"`
	MirroredImages int    `json:"mirroredImages"`
	PendingImages  int    `json:"pendingImages"`
	FailedImages   int    `json:"failedImages"`
}

// ConditionSummary is a condensed view of a metav1.Condition for the API.
type ConditionSummary struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// CatalogSummary describes a single operator catalog tracked by a MirrorTarget.
type CatalogSummary struct {
	Slug                string `json:"slug"`
	Source              string `json:"source"`
	TargetImage         string `json:"targetImage"`
	FilteredPackagesURL string `json:"filteredPackagesUrl"`
	UpstreamPackagesURL string `json:"upstreamPackagesUrl"`
}

// TargetDetail is the JSON response for a single target detail endpoint.
type TargetDetail struct {
	Namespace      string                `json:"namespace"`
	Name           string                `json:"name"`
	Registry       string                `json:"registry"`
	TotalImages    int                   `json:"totalImages"`
	MirroredImages int                   `json:"mirroredImages"`
	PendingImages  int                   `json:"pendingImages"`
	FailedImages   int                   `json:"failedImages"`
	Conditions     []ConditionSummary    `json:"conditions"`
	ImageSets      []ImageSetSummaryJSON `json:"imageSets"`
	Resources      []ResourceLink        `json:"resources"`
	Catalogs       []CatalogSummary      `json:"catalogs"`
}

// ImageSetSummaryJSON is the per-ImageSet status info returned in JSON.
type ImageSetSummaryJSON struct {
	Name        string         `json:"name"`
	Found       bool           `json:"found"`
	Total       int            `json:"total"`
	Mirrored    int            `json:"mirrored"`
	Pending     int            `json:"pending"`
	Failed      int            `json:"failed"`
	Resources   []ResourceLink `json:"resources"`
	HasPlatform bool           `json:"hasPlatform"`
}

// ResourceLink describes a downloadable resource.
type ResourceLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

// FailedImageDetail describes a single failed or pending image with full details.
type FailedImageDetail struct {
	Destination       string `json:"destination"`
	Source            string `json:"source"`
	State             string `json:"state"`
	LastError         string `json:"lastError,omitempty"`
	RetryCount        int    `json:"retryCount,omitempty"`
	PermanentlyFailed bool   `json:"permanentlyFailed,omitempty"`
	ImageSet          string `json:"imageSet"`
}

// ImageFailuresResponse is the JSON response for the image failures endpoint.
type ImageFailuresResponse struct {
	Failed  []FailedImageDetail `json:"failed"`
	Pending []FailedImageDetail `json:"pending"`
}

// RegisterAPIRoutes registers all JSON API and raw-resource routes onto r.
// It does not add any static-asset handler, so callers can append their own.
func (s *Server) RegisterAPIRoutes(r *mux.Router) {
	// Legacy redirect
	r.PathPrefix("/resources/{imageset}/").HandlerFunc(s.handleLegacyRedirect)

	// API endpoints – JSON metadata
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/targets", s.handleTargetsList).Methods("GET")
	api.HandleFunc("/targets/{mt}", s.handleTargetDetail).Methods("GET")
	api.HandleFunc("/targets/{mt}/image-failures", s.handleImageFailures).Methods("GET")

	// Edit endpoints (token-scoped writes — RBAC of the requesting user applies)
	api.HandleFunc("/imagesets/{namespace}/{name}/catalogs/{slug}/packages", s.handleGetPackageConstraints).Methods("GET")
	api.HandleFunc("/imagesets/{namespace}/{name}/catalogs/{slug}/packages", s.handlePatchCatalogPackages).Methods("PATCH")
	api.HandleFunc("/imagesets/{namespace}/{name}/recollect", s.handleTriggerRecollect).Methods("PATCH")
	api.HandleFunc("/releases/channels", s.handleGetOCPChannels).Methods("GET")
	api.HandleFunc("/imagesets/{namespace}/{name}/releases", s.handleGetReleases).Methods("GET")
	api.HandleFunc("/imagesets/{namespace}/{name}/releases", s.handlePatchReleases).Methods("PATCH")
	api.HandleFunc("/imagesets/{namespace}/{name}", s.handleDeleteImageSet).Methods("DELETE")

	// Catalog browsing endpoints
	api.HandleFunc("/targets/{mt}/catalogs/{slug}/packages.json", s.handleFilteredCatalogPackages).Methods("GET")
	api.HandleFunc("/targets/{mt}/catalogs/{slug}/upstream-packages.json", s.handleUpstreamCatalogPackages).Methods("GET")

	// Release GPG signatures
	api.HandleFunc("/targets/{mt}/signatures.yaml", s.handleSignatures).Methods("GET")

	// Raw resource endpoints – YAML/JSON from ConfigMaps (legacy {is} segment kept for compat)
	api.HandleFunc("/targets/{mt}/imagesets/{is}/idms.yaml", s.handleIDMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/itms.yaml", s.handleITMS).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/catalogsource.yaml", s.handleCatalogSource).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/clustercatalog.yaml", s.handleClusterCatalog).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/upstream-packages.json", s.handleUpstreamCatalogPackages).Methods("GET")
	api.HandleFunc("/targets/{mt}/imagesets/{is}/catalogs/{slug}/packages.json", s.handleFilteredCatalogPackages).Methods("GET")
}

func (s *Server) RegisterRoutes(r *mux.Router) {
	s.RegisterAPIRoutes(r)
}

// RegisterPluginStaticRoutes appends a catch-all route that serves embedded
// Console Plugin static assets. Must be called after RegisterAPIRoutes so that
// the more-specific API routes take precedence.
func RegisterPluginStaticRoutes(r *mux.Router) {
	pluginSub, err := fs.Sub(pluginFS, "plugin")
	if err != nil {
		fmt.Printf("RegisterPluginStaticRoutes: failed to sub pluginFS: %v\n", err)
		return
	}
	r.PathPrefix("/").Handler(http.FileServer(http.FS(pluginSub)))
}

func (s *Server) Run(ctx context.Context) {
	s.RunOn(ctx, ":8081")
}

// RunOn starts the HTTP server on the given address and blocks until ctx is done.
func (s *Server) RunOn(ctx context.Context, addr string) {
	r := mux.NewRouter()
	s.RegisterRoutes(r)

	srv := &http.Server{
		Addr:              addr,
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

	fmt.Printf("Resource API started on %s\n", addr)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// --- JSON API handlers ---

func (s *Server) handleTargetsList(w http.ResponseWriter, r *http.Request) {
	c := s.clientForRequest(r)
	list := &mirrorv1alpha1.MirrorTargetList{}
	listOpts := []client.ListOption{}
	if s.namespace != "" {
		listOpts = append(listOpts, client.InNamespace(s.namespace))
	}
	if err := c.List(r.Context(), list, listOpts...); err != nil {
		fmt.Printf("handleTargetsList: client.List failed: %v\n", err)
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("failed to list MirrorTargets: %v", err), http.StatusInternalServerError)
		}
		return
	}
	fmt.Printf("handleTargetsList: found %d targets\n", len(list.Items))

	targets := make([]TargetSummary, 0, len(list.Items))
	for _, mt := range list.Items {
		targets = append(targets, TargetSummary{
			Namespace:      mt.Namespace,
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
	c := s.clientForRequest(r)
	vars := mux.Vars(r)
	mtName := vars["mt"]

	mt, err := s.LookupMirrorTarget(r.Context(), c, mtName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "MirrorTarget not found", http.StatusNotFound)
		} else if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("get MirrorTarget: %v", err), http.StatusInternalServerError)
		}
		return
	}
	ns := mt.Namespace

	// Build resource links from the resources ConfigMap
	cmName := fmt.Sprintf("oc-mirror-%s-resources", mtName)
	cm := &corev1.ConfigMap{}
	resources := make([]ResourceLink, 0)
	if err := c.Get(r.Context(), client.ObjectKey{Name: cmName, Namespace: ns}, cm); err == nil {
		resources = buildResourceLinks(mtName, cm)
	}

	// Add signature resource link if the signatures ConfigMap exists and has data.
	sigCM := &corev1.ConfigMap{}
	if err := c.Get(r.Context(), client.ObjectKey{Name: mtName + "-signatures", Namespace: ns}, sigCM); err == nil && len(sigCM.BinaryData) > 0 {
		resources = append(resources, ResourceLink{
			Name: fmt.Sprintf("Signatures (%d releases)", len(sigCM.BinaryData)),
			URL:  fmt.Sprintf("/api/v1/targets/%s/signatures.yaml", mtName),
			Type: "yaml",
		})
	}

	// Discover per-catalog ConfigMaps to build catalog summaries.
	catalogCMs := &corev1.ConfigMapList{}
	catalogs := make([]CatalogSummary, 0)
	if err := c.List(r.Context(), catalogCMs,
		client.InNamespace(ns),
		client.MatchingLabels{"oc-mirror.openshift.io/mirrortarget": mtName},
	); err == nil {
		seen := make(map[string]bool)
		for _, pcm := range catalogCMs.Items {
			slug, ok := pcm.Labels["oc-mirror.openshift.io/catalog-packages"]
			if !ok || slug == "" {
				continue
			}
			if seen[slug] {
				continue
			}
			seen[slug] = true
			base := fmt.Sprintf("/api/v1/targets/%s/catalogs/%s", mtName, slug)
			catalogs = append(catalogs, CatalogSummary{
				Slug:                slug,
				FilteredPackagesURL: base + "/packages.json",
				UpstreamPackagesURL: base + "/upstream-packages.json",
			})
			resources = append(resources, ResourceLink{
				Name: fmt.Sprintf("Packages (%s)", slug),
				URL:  base + "/packages.json",
				Type: "json",
			})
		}
	}

	// Map CRD conditions to summary structs.
	conditions := make([]ConditionSummary, 0, len(mt.Status.Conditions))
	for _, c := range mt.Status.Conditions {
		conditions = append(conditions, ConditionSummary{
			Type:    c.Type,
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	hasPlatformMap := make(map[string]bool, len(mt.Spec.ImageSets))
	for _, isName := range mt.Spec.ImageSets {
		var imgSet mirrorv1alpha1.ImageSet
		if err := c.Get(r.Context(), client.ObjectKey{Namespace: mt.Namespace, Name: isName}, &imgSet); err == nil {
			hasPlatformMap[isName] = len(imgSet.Spec.Mirror.Platform.Channels) > 0
		}
	}

	imageSets := make([]ImageSetSummaryJSON, 0, len(mt.Status.ImageSetStatuses))
	for _, iss := range mt.Status.ImageSetStatuses {
		imageSets = append(imageSets, ImageSetSummaryJSON{
			Name:        iss.Name,
			Found:       iss.Found,
			Total:       iss.Total,
			Mirrored:    iss.Mirrored,
			Pending:     iss.Pending,
			Failed:      iss.Failed,
			Resources:   []ResourceLink{}, // ImageSet specific resources are currently managed at the target level
			HasPlatform: hasPlatformMap[iss.Name],
		})
	}

	detail := TargetDetail{
		Namespace:      ns,
		Name:           mt.Name,
		Registry:       mt.Spec.Registry,
		TotalImages:    mt.Status.TotalImages,
		MirroredImages: mt.Status.MirroredImages,
		PendingImages:  mt.Status.PendingImages,
		FailedImages:   mt.Status.FailedImages,
		Conditions:     conditions,
		ImageSets:      imageSets,
		Resources:      resources,
		Catalogs:       catalogs,
	}

	writeJSON(w, detail)
}

func (s *Server) handleImageFailures(w http.ResponseWriter, r *http.Request) {
	c := s.clientForRequest(r)
	vars := mux.Vars(r)
	mtName := vars["mt"]

	mt, err := s.LookupMirrorTarget(r.Context(), c, mtName)
	if err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else if apierrors.IsNotFound(err) {
			http.Error(w, "MirrorTarget not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get MirrorTarget: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Load the consolidated ImageState for the MirrorTarget.
	state, err := imagestate.LoadForTarget(r.Context(), c, mt.Namespace, mtName)
	if err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("Failed to load image state: %v", err), http.StatusInternalServerError)
		}
		return
	}

	var failed, pending []FailedImageDetail

	// Iterate through all images and collect failed/pending ones.
	for destination, entry := range state {
		if entry == nil {
			continue
		}

		// Skip mirrored images.
		if entry.State == "Mirrored" {
			continue
		}

		// Determine which ImageSet(s) this entry belongs to.
		// If Refs is populated (new format), use it; otherwise fall back to legacy fields.
		imageSetNames := entry.ImageSetNames()
		if len(imageSetNames) == 0 && entry.Origin != "" {
			// Backward compatibility: legacy entry without Refs.
			imageSetNames = []string{"unknown"}
		}

		// Create a detail entry for each ImageSet that references this image.
		// If there are no ImageSet names, skip it.
		if len(imageSetNames) == 0 {
			continue
		}

		for _, isName := range imageSetNames {
			detail := FailedImageDetail{
				Destination:       destination,
				Source:            entry.Source,
				State:             entry.State,
				LastError:         entry.LastError,
				RetryCount:        entry.RetryCount,
				PermanentlyFailed: entry.PermanentlyFailed,
				ImageSet:          isName,
			}

			// Categorize as failed or pending.
			if entry.PermanentlyFailed {
				failed = append(failed, detail)
			} else {
				pending = append(pending, detail)
			}
		}
	}

	response := ImageFailuresResponse{
		Failed:  failed,
		Pending: pending,
	}

	writeJSON(w, response)
}

// --- Raw resource handlers ---

func (s *Server) handleLegacyRedirect(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusGone)
	_, _ = fmt.Fprintln(w, "Legacy /resources/ paths are deprecated. Please use the new /api/v1/targets/{mt}/imagesets/{is}/... API.")
}

func (s *Server) handleIDMS(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-resources", mtName), "idms.yaml")
}

func (s *Server) handleITMS(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-resources", mtName), "itms.yaml")
}

func (s *Server) handleCatalogSource(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-resources", mtName), fmt.Sprintf("catalogsource-%s.yaml", slug))
}

func (s *Server) handleClusterCatalog(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-resources", mtName), fmt.Sprintf("clustercatalog-%s.yaml", slug))
}

func (s *Server) handleFilteredCatalogPackages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-%s-packages", mtName, slug), "packages.json")
}

func (s *Server) handleUpstreamCatalogPackages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	mtName := vars["mt"]
	slug := vars["slug"]
	c := s.clientForRequest(r)
	ns := s.resolveNamespace(r, c, mtName)
	s.serveConfigMapResource(w, r, ns, fmt.Sprintf("oc-mirror-%s-%s-upstream-packages", mtName, slug), "packages.json")
}

func (s *Server) handleSignatures(w http.ResponseWriter, r *http.Request) {
	c := s.clientForRequest(r)
	vars := mux.Vars(r)
	mtName := vars["mt"]

	mt, err := s.LookupMirrorTarget(r.Context(), c, mtName)
	if err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else if apierrors.IsNotFound(err) {
			http.Error(w, "MirrorTarget not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get MirrorTarget: %v", err), http.StatusInternalServerError)
		}
		return
	}

	cm := &corev1.ConfigMap{}
	if err := c.Get(r.Context(), client.ObjectKey{Name: mtName + "-signatures", Namespace: mt.Namespace}, cm); err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, "Signatures not found", http.StatusNotFound)
		}
		return
	}

	// BinaryData keys are stored as "sha256-<hex>" (colon replaced for ConfigMap compatibility).
	// GenerateSignatureConfigMapsBase64 expects "sha256:<hex>" keys and base64-encoded values.
	b64sigs := make(map[string]string, len(cm.BinaryData))
	for k, v := range cm.BinaryData {
		digest := strings.Replace(k, "sha256-", "sha256:", 1)
		b64sigs[digest] = base64.StdEncoding.EncodeToString(v)
	}

	out, err := mirrorresources.GenerateSignatureConfigMapsBase64(b64sigs)
	if err != nil {
		http.Error(w, "Failed to generate signatures YAML", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(out)
}

// resolveNamespace returns the server's fixed namespace, or looks up the
// MirrorTarget's namespace in cluster-wide mode. Falls back to "" on error so
// the downstream ConfigMap Get will fail naturally with a not-found response.
func (s *Server) resolveNamespace(r *http.Request, c client.Client, mtName string) string {
	if s.namespace != "" {
		return s.namespace
	}
	mt, err := s.LookupMirrorTarget(r.Context(), c, mtName)
	if err != nil {
		return ""
	}
	return mt.Namespace
}

func (s *Server) serveConfigMapResource(w http.ResponseWriter, r *http.Request, namespace, cmName, key string) {
	c := s.clientForRequest(r)
	cm := &corev1.ConfigMap{}
	err := c.Get(r.Context(), client.ObjectKey{Name: cmName, Namespace: namespace}, cm)
	if err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, "Resource not found", http.StatusNotFound)
		}
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
	links := make([]ResourceLink, 0)
	base := fmt.Sprintf("/api/v1/targets/%s/imagesets/latest", mtName)

	if _, ok := cm.Data["idms.yaml"]; ok {
		links = append(links, ResourceLink{Name: "IDMS", URL: base + "/idms.yaml", Type: "yaml"})
	}
	if _, ok := cm.Data["itms.yaml"]; ok {
		links = append(links, ResourceLink{Name: "ITMS", URL: base + "/itms.yaml", Type: "yaml"})
	}

	// Detect catalog resources by key pattern catalogsource-<slug>.yaml or clustercatalog-<slug>.yaml
	for key := range cm.Data {
		if strings.HasSuffix(key, ".yaml") {
			if strings.HasPrefix(key, "catalogsource-") {
				slug := strings.TrimSuffix(strings.TrimPrefix(key, "catalogsource-"), ".yaml")
				links = append(links, ResourceLink{
					Name: fmt.Sprintf("CatalogSource (%s)", slug),
					URL:  fmt.Sprintf("%s/catalogs/%s/catalogsource.yaml", base, slug),
					Type: "yaml",
				})
			} else if strings.HasPrefix(key, "clustercatalog-") {
				slug := strings.TrimSuffix(strings.TrimPrefix(key, "clustercatalog-"), ".yaml")
				links = append(links, ResourceLink{
					Name: fmt.Sprintf("ClusterCatalog (%s)", slug),
					URL:  fmt.Sprintf("%s/catalogs/%s/clustercatalog.yaml", base, slug),
					Type: "yaml",
				})
			}
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

// --- Edit handlers ---

type packageChannelConstraint struct {
	Name       string `json:"name"`
	MinVersion string `json:"minVersion,omitempty"`
	MaxVersion string `json:"maxVersion,omitempty"`
}

type packageConstraint struct {
	Name       string                     `json:"name"`
	MinVersion string                     `json:"minVersion,omitempty"`
	MaxVersion string                     `json:"maxVersion,omitempty"`
	Channels   []packageChannelConstraint `json:"channels,omitempty"`
}

type packagePatchBody struct {
	// Packages carries per-package version constraints (new format).
	Packages []packageConstraint `json:"packages,omitempty"`
	// Include is the legacy simple-name list (no version constraints).
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude"`
}

type releaseChannelConstraint struct {
	Name         string `json:"name"`
	Type         string `json:"type,omitempty"`
	MinVersion   string `json:"minVersion,omitempty"`
	MaxVersion   string `json:"maxVersion,omitempty"`
	ShortestPath bool   `json:"shortestPath,omitempty"`
	Full         bool   `json:"full,omitempty"`
}

type releaseSpec struct {
	Graph         bool                       `json:"graph"`
	Architectures []string                   `json:"architectures,omitempty"`
	Channels      []releaseChannelConstraint `json:"channels"`
}

// handlePatchCatalogPackages updates the package filter for an operator catalog
// in the named ImageSet. The calling user's RBAC rights are enforced via the
// token-scoped client (see clientForRequest).
func (s *Server) handlePatchCatalogPackages(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name, slug := vars["namespace"], vars["name"], vars["slug"]

	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var patch packagePatchBody
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	found := false
	for i, op := range is.Spec.Mirror.Operators {
		catalogSlug := mirrorresources.CatalogSlug(op.Catalog)
		if catalogSlug != slug {
			continue
		}
		found = true
		// Rebuild the package list. Prefer the extended `packages` format
		// (which carries per-channel version constraints) over the legacy
		// `include` string array.
		var packages []mirrorv1alpha1.IncludePackage
		if len(patch.Packages) > 0 {
			for _, pc := range patch.Packages {
				p := mirrorv1alpha1.IncludePackage{
					Name: pc.Name,
					IncludeBundle: mirrorv1alpha1.IncludeBundle{
						MinVersion: pc.MinVersion,
						MaxVersion: pc.MaxVersion,
					},
				}
				for _, ch := range pc.Channels {
					p.Channels = append(p.Channels, mirrorv1alpha1.IncludeChannel{
						Name: ch.Name,
						IncludeBundle: mirrorv1alpha1.IncludeBundle{
							MinVersion: ch.MinVersion,
							MaxVersion: ch.MaxVersion,
						},
					})
				}
				packages = append(packages, p)
			}
		} else {
			for _, name := range patch.Include {
				packages = append(packages, mirrorv1alpha1.IncludePackage{Name: name})
			}
		}
		is.Spec.Mirror.Operators[i].Packages = packages
	}

	if !found {
		http.Error(w, fmt.Sprintf("catalog slug %q not found in ImageSet", slug), http.StatusNotFound)
		return
	}

	if err := c.Update(r.Context(), is); err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("update ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGetPackageConstraints returns the current per-package version constraints
// stored in the ImageSet spec for a given catalog slug.
func (s *Server) handleGetPackageConstraints(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name, slug := vars["namespace"], vars["name"], vars["slug"]

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	var result []packageConstraint
	for _, op := range is.Spec.Mirror.Operators {
		if mirrorresources.CatalogSlug(op.Catalog) != slug {
			continue
		}
		for _, pkg := range op.Packages {
			pc := packageConstraint{
				Name:       pkg.Name,
				MinVersion: pkg.MinVersion,
				MaxVersion: pkg.MaxVersion,
			}
			for _, ch := range pkg.Channels {
				pc.Channels = append(pc.Channels, packageChannelConstraint{
					Name:       ch.Name,
					MinVersion: ch.MinVersion,
					MaxVersion: ch.MaxVersion,
				})
			}
			result = append(result, pc)
		}
	}
	if result == nil {
		result = []packageConstraint{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// handleTriggerRecollect sets the recollect annotation on an ImageSet to force
// an upstream re-resolution on the next Manager reconcile cycle.
func (s *Server) handleTriggerRecollect(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name := vars["namespace"], vars["name"]

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	if is.Annotations == nil {
		is.Annotations = make(map[string]string)
	}
	is.Annotations["mirror.openshift.io/recollect"] = "true"

	if err := c.Update(r.Context(), is); err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("update ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteImageSet deletes an ImageSet using the caller's token.
func (s *Server) handleDeleteImageSet(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name := vars["namespace"], vars["name"]

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	if err := c.Delete(r.Context(), is); err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("delete ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleGetReleases returns the current platform/release configuration from the ImageSet spec.
func (s *Server) handleGetReleases(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name := vars["namespace"], vars["name"]

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	p := is.Spec.Mirror.Platform
	resp := releaseSpec{
		Graph:         p.Graph,
		Architectures: p.Architectures,
		Channels:      []releaseChannelConstraint{},
	}
	for _, ch := range p.Channels {
		resp.Channels = append(resp.Channels, releaseChannelConstraint{
			Name:         ch.Name,
			Type:         string(ch.Type),
			MinVersion:   ch.MinVersion,
			MaxVersion:   ch.MaxVersion,
			ShortestPath: ch.ShortestPath,
			Full:         ch.Full,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handlePatchReleases updates the platform/release configuration in the ImageSet spec.
func (s *Server) handlePatchReleases(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	namespace, name := vars["namespace"], vars["name"]

	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var patch releaseSpec
	if err := json.Unmarshal(body, &patch); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	c := s.clientForRequest(r)
	is := &mirrorv1alpha1.ImageSet{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, is); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "ImageSet not found", http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("get ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	is.Spec.Mirror.Platform.Graph = patch.Graph
	if patch.Architectures != nil {
		is.Spec.Mirror.Platform.Architectures = patch.Architectures
	}

	channels := make([]mirrorv1alpha1.ReleaseChannel, 0, len(patch.Channels))
	for _, ch := range patch.Channels {
		pt := mirrorv1alpha1.PlatformType(ch.Type)
		if pt == "" {
			pt = mirrorv1alpha1.TypeOCP
		}
		channels = append(channels, mirrorv1alpha1.ReleaseChannel{
			Name:         ch.Name,
			Type:         pt,
			MinVersion:   ch.MinVersion,
			MaxVersion:   ch.MaxVersion,
			ShortestPath: ch.ShortestPath,
			Full:         ch.Full,
		})
	}
	is.Spec.Mirror.Platform.Channels = channels

	if err := c.Update(r.Context(), is); err != nil {
		if apierrors.IsForbidden(err) {
			http.Error(w, "forbidden: insufficient permissions", http.StatusForbidden)
		} else {
			http.Error(w, fmt.Sprintf("update ImageSet: %v", err), http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// getOCPChannels returns the list of available OCP release channels. Sources
// are tried in order:
//  1. In-memory cache (TTL: 1 hour)
//  2. GitHub openshift/cincinnati-graph-data repository (live, requires internet)
//  3. ConfigMap "oc-mirror-ocp-versions" in the operator namespace (air-gap override)
//  4. Built-in hardcoded defaults (4.14–4.19, stable/fast/eus/candidate)
func (s *Server) getOCPChannels(ctx context.Context) []ocpChannelEntry {
	s.channelCache.mu.Lock()
	defer s.channelCache.mu.Unlock()

	if s.channelCache.entries != nil && time.Since(s.channelCache.fetchedAt) < channelCacheTTL {
		return s.channelCache.entries
	}

	if entries, err := s.fetchChannelsFromGitHub(ctx); err == nil {
		s.channelCache.entries = entries
		s.channelCache.fetchedAt = time.Now()
		return entries
	}

	if entries, err := s.fetchChannelsFromConfigMap(ctx); err == nil {
		s.channelCache.entries = entries
		s.channelCache.fetchedAt = time.Now()
		return entries
	}

	return defaultOCPChannels
}

// fetchChannelsFromGitHub queries the GitHub Contents API for the
// openshift/cincinnati-graph-data channels directory and parses channel names
// from YAML filenames (e.g. "stable-4.18.yaml" → name="stable-4.18", version="4.18").
func (s *Server) fetchChannelsFromGitHub(ctx context.Context) ([]ocpChannelEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubGraphDataChannelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "oc-mirror-operator/resourceapi")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github API request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var contents []githubContentEntry
	if err := json.Unmarshal(body, &contents); err != nil {
		return nil, err
	}

	entries := make([]ocpChannelEntry, 0, len(contents))
	for _, f := range contents {
		if f.Type != "file" || !strings.HasSuffix(f.Name, ".yaml") {
			continue
		}
		// Channel files follow the pattern "<type>-<major>.<minor>.yaml"
		// e.g. "stable-4.18.yaml", "eus-4.16.yaml", "okd-4.14.yaml"
		baseName := strings.TrimSuffix(f.Name, ".yaml")
		lastDash := strings.LastIndex(baseName, "-")
		if lastDash < 0 {
			continue
		}
		chType := baseName[:lastDash]
		ver := baseName[lastDash+1:]
		if !strings.Contains(ver, ".") {
			continue
		}
		platformType := "ocp"
		if chType == platformTypeOKD {
			platformType = platformTypeOKD
		}
		entries = append(entries, ocpChannelEntry{
			Name:    baseName,
			Type:    platformType,
			Version: ver,
		})
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no channel entries parsed from GitHub response")
	}
	return entries, nil
}

// fetchChannelsFromConfigMap reads the optional "oc-mirror-ocp-versions"
// ConfigMap from the operator namespace. Expected structure:
//
//	data:
//	  versions: "4.16,4.17,4.18,4.19"          # comma-separated OCP minor versions
//	  channelTypes: "stable,fast,eus,candidate"  # defaults to all four types
//
// This ConfigMap acts as an air-gap override: when the operator cannot reach
// api.github.com, administrators configure the allowed version range manually.
func (s *Server) fetchChannelsFromConfigMap(ctx context.Context) ([]ocpChannelEntry, error) {
	if s.namespace == "" {
		return nil, fmt.Errorf("no namespace configured for ConfigMap lookup")
	}

	cm := &corev1.ConfigMap{}
	if err := s.client.Get(ctx, client.ObjectKey{
		Namespace: s.namespace,
		Name:      "oc-mirror-ocp-versions",
	}, cm); err != nil {
		return nil, err
	}

	versionsStr := strings.TrimSpace(cm.Data["versions"])
	if versionsStr == "" {
		return nil, fmt.Errorf("ConfigMap oc-mirror-ocp-versions has no 'versions' key")
	}
	channelTypesStr := strings.TrimSpace(cm.Data["channelTypes"])
	if channelTypesStr == "" {
		channelTypesStr = "stable,fast,eus,candidate"
	}

	var entries []ocpChannelEntry
	for _, ver := range strings.Split(versionsStr, ",") {
		ver = strings.TrimSpace(ver)
		if ver == "" || !strings.Contains(ver, ".") {
			continue
		}
		minor, _ := strconv.Atoi(strings.Split(ver, ".")[1])
		for _, t := range strings.Split(channelTypesStr, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if t == "eus" && minor%2 != 0 {
				continue
			}
			platformType := "ocp"
			if t == platformTypeOKD {
				platformType = platformTypeOKD
			}
			entries = append(entries, ocpChannelEntry{
				Name:    t + "-" + ver,
				Type:    platformType,
				Version: ver,
			})
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no channel entries derived from ConfigMap")
	}
	return entries, nil
}

// handleGetOCPChannels returns the list of available OCP release channels for
// the release browser UI. Results are fetched from the openshift/cincinnati-graph-data
// GitHub repository (cached 1h), with a ConfigMap override for air-gapped clusters
// and a built-in fallback list as last resort.
func (s *Server) handleGetOCPChannels(w http.ResponseWriter, r *http.Request) {
	entries := s.getOCPChannels(r.Context())
	writeJSON(w, entries)
}
