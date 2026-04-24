package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/imagestate"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Server serves generated cluster resources via HTTP.
type Server struct {
	client         client.Client
	namespace      string
	target         string // MirrorTarget name
	authConfigPath string // Docker auth config directory (for registry access)
}

// NewServer creates a new resource HTTP server.
func NewServer(cl client.Client, namespace, targetName, authConfigPath string) *Server {
	return &Server{
		client:         cl,
		namespace:      namespace,
		target:         targetName,
		authConfigPath: authConfigPath,
	}
}

// Run starts the resource server on :8081 and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/resources/", s.handleIndex)
	mux.HandleFunc("/resources/{imageset}/idms.yaml", s.handleIDMS)
	mux.HandleFunc("/resources/{imageset}/itms.yaml", s.handleITMS)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/catalogsource.yaml", s.handleCatalogSource)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/clustercatalog.yaml", s.handleClusterCatalog)
	mux.HandleFunc("/resources/{imageset}/catalogs/{catalog}/packages.json", s.handleCatalogPackages)
	mux.HandleFunc("/resources/{imageset}/signature-configmaps.yaml", s.handleSignatures)

	server := &http.Server{
		Addr:              ":8081",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		fmt.Println("Resource server listening on :8081")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Resource server failed: %v\n", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

// --- handlers ---

// resourceIndex describes available resources for an ImageSet.
type resourceIndex struct {
	ImageSets []imageSetIndex `json:"imageSets"`
}

type imageSetIndex struct {
	Name      string   `json:"name"`
	Ready     bool     `json:"ready"`
	Resources []string `json:"resources"`
	Catalogs  []string `json:"catalogs,omitempty"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Only handle exact /resources/ path
	if r.URL.Path != "/resources/" && r.URL.Path != "/resources" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	imageSets, err := s.listImageSets(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mt, err := s.getMirrorTarget(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	idx := resourceIndex{}
	for _, is := range imageSets {
		ready := isImageSetReady(&is)
		entry := imageSetIndex{
			Name:  is.Name,
			Ready: ready,
			Resources: []string{
				fmt.Sprintf("/resources/%s/idms.yaml", is.Name),
				fmt.Sprintf("/resources/%s/itms.yaml", is.Name),
				fmt.Sprintf("/resources/%s/signature-configmaps.yaml", is.Name),
			},
		}
		for _, cat := range extractCatalogs(&is, mt.Spec.Registry) {
			catName := catalogSlug(cat.SourceCatalog)
			entry.Catalogs = append(entry.Catalogs, catName)
			entry.Resources = append(entry.Resources,
				fmt.Sprintf("/resources/%s/catalogs/%s/catalogsource.yaml", is.Name, catName),
				fmt.Sprintf("/resources/%s/catalogs/%s/clustercatalog.yaml", is.Name, catName),
				fmt.Sprintf("/resources/%s/catalogs/%s/packages.json", is.Name, catName),
			)
		}
		idx.ImageSets = append(idx.ImageSets, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(idx)
}

func (s *Server) handleIDMS(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")
	state, err := s.loadReadyState(r.Context(), isName)
	if err != nil {
		if strings.Contains(err.Error(), "not ready") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	data, err := GenerateIDMS(isName, state)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(data)
}

func (s *Server) handleITMS(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")
	state, err := s.loadReadyState(r.Context(), isName)
	if err != nil {
		if strings.Contains(err.Error(), "not ready") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	data, err := GenerateITMS(isName, state)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(data)
}

func (s *Server) handleCatalogSource(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")
	catSlug := r.PathValue("catalog")

	is, err := s.getImageSet(r.Context(), isName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	mt, err := s.getMirrorTarget(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	catalogs := extractCatalogs(is, mt.Spec.Registry)
	cat, ok := findCatalog(catalogs, catSlug)
	if !ok {
		http.Error(w, fmt.Sprintf("catalog %q not found in ImageSet %s", catSlug, isName), http.StatusNotFound)
		return
	}

	csName := fmt.Sprintf("oc-mirror-%s", catSlug)
	data, err := GenerateCatalogSource(csName, "openshift-marketplace", cat, mt.Spec.AuthSecret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(data)
}

func (s *Server) handleClusterCatalog(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")
	catSlug := r.PathValue("catalog")

	is, err := s.getImageSet(r.Context(), isName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	mt, err := s.getMirrorTarget(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	catalogs := extractCatalogs(is, mt.Spec.Registry)
	cat, ok := findCatalog(catalogs, catSlug)
	if !ok {
		http.Error(w, fmt.Sprintf("catalog %q not found in ImageSet %s", catSlug, isName), http.StatusNotFound)
		return
	}

	ccName := fmt.Sprintf("oc-mirror-%s", catSlug)
	data, err := GenerateClusterCatalog(ccName, cat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(data)
}

func (s *Server) handleCatalogPackages(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")
	catSlug := r.PathValue("catalog")

	is, err := s.getImageSet(r.Context(), isName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Gate on CatalogReady — the target catalog must be built before we can
	// inspect it. This avoids showing unfiltered upstream content that may
	// not actually be available in the mirror.
	if !isCatalogReady(is) {
		http.Error(w, fmt.Sprintf("catalog for ImageSet %s is not ready yet", isName), http.StatusConflict)
		return
	}

	mt, err := s.getMirrorTarget(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	catalogs := extractCatalogs(is, mt.Spec.Registry)
	cat, ok := findCatalog(catalogs, catSlug)
	if !ok {
		http.Error(w, fmt.Sprintf("catalog %q not found in ImageSet %s", catSlug, isName), http.StatusNotFound)
		return
	}

	// Build a registry client and load FBC from the target (mirrored) catalog image.
	var insecureHosts []string
	if mt.Spec.Insecure {
		host := mt.Spec.Registry
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		insecureHosts = []string{host}
	}
	mc := mirrorclient.NewMirrorClient(insecureHosts, s.authConfigPath)
	resolver := catalog.New(mc)

	cfg, err := resolver.LoadFBC(r.Context(), cat.TargetImage)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load catalog from %s: %v", cat.TargetImage, err), http.StatusInternalServerError)
		return
	}

	resp := buildCatalogPackagesResponse(cat, cfg)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSignatures(w http.ResponseWriter, r *http.Request) {
	isName := r.PathValue("imageset")

	// Load signature ConfigMap if it exists.
	sigCM := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: fmt.Sprintf("%s-signatures", isName), Namespace: s.namespace}
	if err := s.client.Get(r.Context(), key, sigCM); err != nil {
		w.Header().Set("Content-Type", "text/yaml")
		_, _ = w.Write([]byte("# No release signatures available yet\n"))
		return
	}

	sigs := make(SignatureData)
	for k, v := range sigCM.BinaryData {
		// Key format: sha256-<hash>
		digest := strings.Replace(k, "-", ":", 1)
		sigs[digest] = v
	}

	data, err := GenerateSignatureConfigMaps(sigs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	_, _ = w.Write(data)
}

func (s *Server) listImageSets(ctx context.Context) ([]mirrorv1alpha1.ImageSet, error) {
	// Find the MirrorTarget to get its imageSets list.
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.target, Namespace: s.namespace}, mt); err != nil {
		return nil, fmt.Errorf("failed to get MirrorTarget %s: %w", s.target, err)
	}

	list := &mirrorv1alpha1.ImageSetList{}
	if err := s.client.List(ctx, list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}

	allowed := make(map[string]struct{}, len(mt.Spec.ImageSets))
	for _, name := range mt.Spec.ImageSets {
		allowed[name] = struct{}{}
	}

	var result []mirrorv1alpha1.ImageSet
	for _, is := range list.Items {
		if _, ok := allowed[is.Name]; ok {
			result = append(result, is)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *Server) getImageSet(ctx context.Context, name string) (*mirrorv1alpha1.ImageSet, error) {
	// Verify the ImageSet is in the MirrorTarget's list.
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.target, Namespace: s.namespace}, mt); err != nil {
		return nil, fmt.Errorf("failed to get MirrorTarget %s: %w", s.target, err)
	}

	found := false
	for _, isName := range mt.Spec.ImageSets {
		if isName == name {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("ImageSet %s is not listed in MirrorTarget %s", name, s.target)
	}

	is := &mirrorv1alpha1.ImageSet{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: name, Namespace: s.namespace}, is); err != nil {
		return nil, err
	}
	return is, nil
}

func (s *Server) getMirrorTarget(ctx context.Context) (*mirrorv1alpha1.MirrorTarget, error) {
	mt := &mirrorv1alpha1.MirrorTarget{}
	if err := s.client.Get(ctx, client.ObjectKey{Name: s.target, Namespace: s.namespace}, mt); err != nil {
		return nil, err
	}
	return mt, nil
}

func (s *Server) loadReadyState(ctx context.Context, isName string) (imagestate.ImageState, error) {
	is, err := s.getImageSet(ctx, isName)
	if err != nil {
		return nil, err
	}
	if !isImageSetReady(is) {
		return nil, fmt.Errorf("ImageSet %s is not ready yet", isName)
	}
	return imagestate.Load(ctx, s.client, s.namespace, isName)
}

func isImageSetReady(is *mirrorv1alpha1.ImageSet) bool {
	for _, c := range is.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

// extractCatalogs derives CatalogInfo from an ImageSet's operator spec and the target registry.
func extractCatalogs(is *mirrorv1alpha1.ImageSet, registryPrefix string) []CatalogInfo {
	result := make([]CatalogInfo, 0, len(is.Spec.Mirror.Operators))
	for _, op := range is.Spec.Mirror.Operators {
		if op.Catalog == "" {
			continue
		}
		result = append(result, CatalogInfo{
			SourceCatalog: op.Catalog,
			TargetImage:   CatalogTargetRef(registryPrefix, op),
			DisplayName:   catalogDisplayName(op.Catalog),
		})
	}
	return result
}

// CatalogTargetRef computes the target catalog image reference.
// This mirrors the controller's catalogTargetRef logic exactly:
// it prefers Operator.TargetCatalog if set; otherwise derives a path
// from the source catalog name and appends TargetTag (defaulting to the
// source tag, or "latest" for digest-only catalog references).
func CatalogTargetRef(registry string, op mirrorv1alpha1.Operator) string {
	tag := op.TargetTag
	if tag == "" {
		// Extract tag from the source catalog, handling "image:tag",
		// "image@sha256:..." and "image:tag@sha256:..." forms.
		catalogForTag := op.Catalog
		if i := strings.Index(catalogForTag, "@"); i >= 0 {
			catalogForTag = catalogForTag[:i] // strip digest, keep tag part
		}
		if i := strings.LastIndex(catalogForTag, ":"); i >= 0 && !strings.Contains(catalogForTag[i:], "/") {
			tag = catalogForTag[i+1:]
		}
	}
	if tag == "" {
		tag = "latest"
	}
	if op.TargetCatalog != "" {
		return fmt.Sprintf("%s/%s:%s", registry, op.TargetCatalog, tag)
	}
	// Derive a safe path from the source catalog: strip registry prefix, keep image name.
	parts := strings.SplitN(op.Catalog, "/", 2)
	path := op.Catalog
	if len(parts) == 2 {
		path = parts[1]
	}
	// Remove tag/digest from path.
	if i := strings.IndexAny(path, ":@"); i >= 0 {
		path = path[:i]
	}
	return fmt.Sprintf("%s/%s:%s", registry, path, tag)
}

// catalogDisplayName derives a human-readable name from a catalog reference.
func catalogDisplayName(source string) string {
	repo := repoOnly(source)
	parts := strings.Split(repo, "/")
	if len(parts) > 0 {
		name := parts[len(parts)-1]
		return fmt.Sprintf("OC Mirror - %s", name)
	}
	return "OC Mirror Catalog"
}

// catalogSlug creates a URL-safe short name from a catalog reference.
func catalogSlug(source string) string {
	repo := repoOnly(source)
	parts := strings.Split(repo, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "unknown"
}

func findCatalog(catalogs []CatalogInfo, slug string) (CatalogInfo, bool) {
	for _, c := range catalogs {
		if catalogSlug(c.SourceCatalog) == slug {
			return c, true
		}
	}
	return CatalogInfo{}, false
}

func isCatalogReady(is *mirrorv1alpha1.ImageSet) bool {
	for _, c := range is.Status.Conditions {
		if c.Type == "CatalogReady" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// --- Catalog packages response types ---

// catalogPackagesResponse is the JSON envelope for the packages endpoint.
type catalogPackagesResponse struct {
	Catalog     string           `json:"catalog"`
	TargetImage string           `json:"targetImage"`
	Packages    []packageSummary `json:"packages"`
}

// packageSummary describes a single operator package in the catalog.
type packageSummary struct {
	Name           string           `json:"name"`
	DefaultChannel string           `json:"defaultChannel,omitempty"`
	Channels       []channelSummary `json:"channels"`
}

// channelSummary describes a single channel within an operator package.
type channelSummary struct {
	Name    string        `json:"name"`
	Entries []bundleEntry `json:"entries"`
}

// bundleEntry describes a single bundle version within a channel.
type bundleEntry struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

func buildCatalogPackagesResponse(cat CatalogInfo, cfg *declcfg.DeclarativeConfig) catalogPackagesResponse {
	// Index bundles and channels by package.
	channelsByPkg := make(map[string][]declcfg.Channel)
	for _, ch := range cfg.Channels {
		channelsByPkg[ch.Package] = append(channelsByPkg[ch.Package], ch)
	}

	// Extract version from olm.package property for each bundle.
	bundleVersions := make(map[string]string, len(cfg.Bundles))
	for _, b := range cfg.Bundles {
		bundleVersions[b.Name] = bundleVersion(b)
	}

	// Build default channel index.
	defaultChannels := make(map[string]string, len(cfg.Packages))
	for _, p := range cfg.Packages {
		defaultChannels[p.Name] = p.DefaultChannel
	}

	// Build sorted package list.
	pkgNames := make([]string, 0, len(cfg.Packages))
	for _, p := range cfg.Packages {
		pkgNames = append(pkgNames, p.Name)
	}
	sort.Strings(pkgNames)

	packages := make([]packageSummary, 0, len(pkgNames))
	for _, pkgName := range pkgNames {
		channels := channelsByPkg[pkgName]
		sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })

		chSummaries := make([]channelSummary, 0, len(channels))
		for _, ch := range channels {
			entries := make([]bundleEntry, 0, len(ch.Entries))
			for _, e := range ch.Entries {
				entries = append(entries, bundleEntry{
					Name:    e.Name,
					Version: bundleVersions[e.Name],
				})
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			chSummaries = append(chSummaries, channelSummary{
				Name:    ch.Name,
				Entries: entries,
			})
		}

		packages = append(packages, packageSummary{
			Name:           pkgName,
			DefaultChannel: defaultChannels[pkgName],
			Channels:       chSummaries,
		})
	}

	return catalogPackagesResponse{
		Catalog:     cat.SourceCatalog,
		TargetImage: cat.TargetImage,
		Packages:    packages,
	}
}

// bundleVersion extracts the version from a bundle's olm.package property.
func bundleVersion(b declcfg.Bundle) string {
	for _, prop := range b.Properties {
		if prop.Type != "olm.package" {
			continue
		}
		var v struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(prop.Value, &v) == nil && v.Version != "" {
			return v.Version
		}
	}
	return ""
}
