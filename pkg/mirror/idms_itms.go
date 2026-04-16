package mirror

import (
	"strings"

	confv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// GenerateIDMS generates an ImageDigestMirrorSet based on mirrored images
func GenerateIDMS(name string, images []TargetImage) ([]byte, error) {
	idms := &confv1.ImageDigestMirrorSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "config.openshift.io/v1",
			Kind:       "ImageDigestMirrorSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: confv1.ImageDigestMirrorSetSpec{
			ImageDigestMirrors: []confv1.ImageDigestMirrors{},
		},
	}

	// Group by source registry/repo
	mirrors := make(map[string]string)
	for _, img := range images {
		// Only for digest-based images (simplification: assume all are or check)
		if strings.Contains(img.Source, "@sha256:") {
			sourceRepo := strings.Split(img.Source, "@")[0]
			destRepo := strings.Split(img.Destination, "@")[0]
			if strings.Contains(img.Destination, ":sha256-") {
				destRepo = strings.Split(img.Destination, ":")[0]
			}
			mirrors[sourceRepo] = destRepo
		}
	}

	for src, dest := range mirrors {
		idms.Spec.ImageDigestMirrors = append(idms.Spec.ImageDigestMirrors, confv1.ImageDigestMirrors{
			Source: src,
			Mirrors: []confv1.ImageMirror{
				confv1.ImageMirror(dest),
			},
		})
	}

	return yaml.Marshal(idms)
}

// GenerateITMS generates an ImageTagMirrorSet based on mirrored images
func GenerateITMS(name string, images []TargetImage) ([]byte, error) {
	itms := &confv1.ImageTagMirrorSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "config.openshift.io/v1",
			Kind:       "ImageTagMirrorSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: confv1.ImageTagMirrorSetSpec{
			ImageTagMirrors: []confv1.ImageTagMirrors{},
		},
	}

	mirrors := make(map[string]string)
	for _, img := range images {
		if strings.Contains(img.Source, ":") && !strings.Contains(img.Source, "@") {
			sourceRepo := strings.Split(img.Source, ":")[0]
			destRepo := strings.Split(img.Destination, ":")[0]
			mirrors[sourceRepo] = destRepo
		}
	}

	for src, dest := range mirrors {
		itms.Spec.ImageTagMirrors = append(itms.Spec.ImageTagMirrors, confv1.ImageTagMirrors{
			Source: src,
			Mirrors: []confv1.ImageMirror{
				confv1.ImageMirror(dest),
			},
		})
	}

	return yaml.Marshal(itms)
}
