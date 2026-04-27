package manager

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	mirrorv1alpha1 "github.com/mariusbertram/oc-mirror-operator/api/v1alpha1"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/catalog"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	"github.com/mariusbertram/oc-mirror-operator/pkg/mirror/resources"
)

const (
	conditionReady = "Ready"
)

// renderResources pre-renders cluster resources (IDMS, ITMS, etc.) for each ImageSet
// and persists them in ConfigMaps with label selector app=oc-mirror-resources.
func (m *MirrorManager) renderResources(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, activeIS []*mirrorv1alpha1.ImageSet) error {
	for _, is := range activeIS {
		if !isImageSetReady(is) {
			continue
		}

		// 1. Generate IDMS/ITMS
		idms, err := resources.GenerateIDMS(is.Name, m.imageState)
		if err == nil {
			if err := m.saveResourceConfigMap(ctx, mt, is.Name, "idms", idms); err != nil {
				fmt.Printf("Warning: failed to save IDMS for %s: %v\n", is.Name, err)
			}
		}

		itms, err := resources.GenerateITMS(is.Name, m.imageState)
		if err == nil {
			if err := m.saveResourceConfigMap(ctx, mt, is.Name, "itms", itms); err != nil {
				fmt.Printf("Warning: failed to save ITMS for %s: %v\n", is.Name, err)
			}
		}

		// 2. Generate Signatures
		sigCMName := is.Name + "-signatures"
		sigCM := &corev1.ConfigMap{}
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: sigCMName}, sigCM); err == nil {
			sigs := make(resources.SignatureData)
			for k, v := range sigCM.BinaryData {
				digest := strings.Replace(k, "-", ":", 1)
				sigs[digest] = v
			}
			sigYAML, err := resources.GenerateSignatureConfigMaps(sigs)
			if err == nil {
				if err := m.saveResourceConfigMap(ctx, mt, is.Name, "signatures", sigYAML); err != nil {
					fmt.Printf("Warning: failed to save signatures for %s: %v\n", is.Name, err)
				}
			}
		}

		// 3. Catalogs
		catalogs := resources.ExtractCatalogs(is, mt.Spec.Registry)
		for _, cat := range catalogs {
			catSlug := resources.CatalogSlug(cat.SourceCatalog)

			// CatalogSource
			csName := fmt.Sprintf("oc-mirror-%s", catSlug)
			csYAML, err := resources.GenerateCatalogSource(csName, "openshift-marketplace", cat, mt.Spec.AuthSecret)
			if err == nil {
				if err := m.saveResourceConfigMap(ctx, mt, is.Name, catSlug+"-catalogsource", csYAML); err != nil {
					fmt.Printf("Warning: failed to save CatalogSource for %s/%s: %v\n", is.Name, catSlug, err)
				}
			}

			// ClusterCatalog
			ccYAML, err := resources.GenerateClusterCatalog(csName, cat)
			if err == nil {
				if err := m.saveResourceConfigMap(ctx, mt, is.Name, catSlug+"-clustercatalog", ccYAML); err != nil {
					fmt.Printf("Warning: failed to save ClusterCatalog for %s/%s: %v\n", is.Name, catSlug, err)
				}
			}

			// Packages (FBC resolved by Manager)
			if isCatalogReady(is) {
				m.renderCatalogPackages(ctx, mt, is, cat, catSlug)
			}
		}
	}

	// 4. Update index ConfigMap
	return m.renderIndex(ctx, mt, activeIS)
}

func (m *MirrorManager) renderCatalogPackages(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, is *mirrorv1alpha1.ImageSet, cat resources.CatalogInfo, catSlug string) {
	// Build registry client for target registry.
	var insecureHosts []string
	if mt.Spec.Insecure {
		host := mt.Spec.Registry
		if i := strings.Index(host, "/"); i >= 0 {
			host = host[:i]
		}
		insecureHosts = []string{host}
	}
	mc := mirrorclient.NewMirrorClient(insecureHosts, m.authConfigPath)
	resolver := catalog.New(mc)

	cfg, err := resolver.LoadFBC(ctx, cat.TargetImage)
	if err != nil {
		fmt.Printf("Warning: failed to load FBC from %s: %v\n", cat.TargetImage, err)
		return
	}

	resp := resources.BuildCatalogPackagesResponse(cat, cfg)
	data, _ := json.Marshal(resp)
	if err := m.saveResourceConfigMap(ctx, mt, is.Name, catSlug+"-packages", data); err != nil {
		fmt.Printf("Warning: failed to save packages for %s/%s: %v\n", is.Name, catSlug, err)
	}

	// Also render upstream packages (cached in Manager)
	mcUp := mirrorclient.NewMirrorClient(nil, m.authConfigPath)
	resUp := catalog.New(mcUp)
	cfgUp, err := resUp.LoadFBC(ctx, cat.SourceCatalog)
	if err == nil {
		respUp := resources.BuildCatalogPackagesResponse(cat, cfgUp)
		dataUp, _ := json.Marshal(respUp)
		if err := m.saveResourceConfigMap(ctx, mt, is.Name, catSlug+"-upstream-packages", dataUp); err != nil {
			fmt.Printf("Warning: failed to save upstream packages for %s/%s: %v\n", is.Name, catSlug, err)
		}
	}
}

func (m *MirrorManager) renderIndex(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, activeIS []*mirrorv1alpha1.ImageSet) error {
	idx := resources.ResourceIndex{}
	for _, is := range activeIS {
		ready := isImageSetReady(is)
		entry := resources.ImageSetIndex{
			Name:  is.Name,
			Ready: ready,
			Resources: []string{
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/idms.yaml", mt.Name, is.Name),
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/itms.yaml", mt.Name, is.Name),
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/signature-configmaps.yaml", mt.Name, is.Name),
			},
		}
		for _, cat := range resources.ExtractCatalogs(is, mt.Spec.Registry) {
			catSlug := resources.CatalogSlug(cat.SourceCatalog)
			entry.Catalogs = append(entry.Catalogs, catSlug)
			entry.Resources = append(entry.Resources,
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/catalogs/%s/catalogsource.yaml", mt.Name, is.Name, catSlug),
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/catalogs/%s/clustercatalog.yaml", mt.Name, is.Name, catSlug),
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/catalogs/%s/packages.json", mt.Name, is.Name, catSlug),
				fmt.Sprintf("/api/v1/targets/%s/imagesets/%s/catalogs/%s/upstream-packages.json", mt.Name, is.Name, catSlug),
			)
		}
		idx.ImageSets = append(idx.ImageSets, entry)
	}

	data, _ := json.Marshal(idx)
	return m.saveResourceConfigMap(ctx, mt, "", "index", data)
}

func (m *MirrorManager) saveResourceConfigMap(ctx context.Context, mt *mirrorv1alpha1.MirrorTarget, isName, resType string, data []byte) error {
	name := fmt.Sprintf("%s-resource-%s", mt.Name, resType)
	if isName != "" {
		name = fmt.Sprintf("%s-resource-%s-%s", mt.Name, isName, resType)
	}

	// Hash-check to avoid unnecessary updates.
	hash := fmt.Sprintf("%x", sha256.Sum256(data))

	existing := &corev1.ConfigMap{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name}, existing)
	if err == nil {
		if existing.Annotations != nil && existing.Annotations["mirror.openshift.io/hash"] == hash {
			return nil // No change
		}
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app":          "oc-mirror-resources",
				"mirrortarget": mt.Name,
			},
			Annotations: map[string]string{
				"mirror.openshift.io/hash": hash,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         mirrorv1alpha1.GroupVersion.String(),
				Kind:               "MirrorTarget",
				Name:               mt.Name,
				UID:                mt.UID,
				Controller:         ptr(true),
				BlockOwnerDeletion: ptr(false),
			}},
		},
		Data: map[string]string{
			"data": string(data),
		},
	}

	if errors.IsNotFound(err) {
		return m.Client.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	cm.ResourceVersion = existing.ResourceVersion
	return m.Client.Update(ctx, cm)
}

func isImageSetReady(is *mirrorv1alpha1.ImageSet) bool {
	for _, c := range is.Status.Conditions {
		if c.Type == conditionReady && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func isCatalogReady(is *mirrorv1alpha1.ImageSet) bool {
	for _, c := range is.Status.Conditions {
		if c.Type == "CatalogReady" && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
