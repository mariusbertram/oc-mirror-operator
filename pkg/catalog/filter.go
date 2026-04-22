package catalog

import (
	"context"
	"errors"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// ErrInvalidInput is returned by FilterCatalog when its inputs are unusable
// (e.g. a nil DeclarativeConfig). The previous implementation returned an
// empty config and nil error, which silently masked programmer errors.
var ErrInvalidInput = errors.New("invalid input")

// FilterCatalog filters an OLM FBC based on the provided package list. It
// returns ErrInvalidInput if fullConfig is nil. An empty packages slice is a
// no-op-style signal: the returned config is empty (callers can treat that as
// "nothing to mirror").
func FilterCatalog(_ context.Context, fullConfig *declcfg.DeclarativeConfig, packages []string) (*declcfg.DeclarativeConfig, error) {
	if fullConfig == nil {
		return nil, ErrInvalidInput
	}

	filtered := &declcfg.DeclarativeConfig{
		Packages: []declcfg.Package{},
		Channels: []declcfg.Channel{},
		Bundles:  []declcfg.Bundle{},
	}

	pkgMap := make(map[string]bool, len(packages))
	for _, p := range packages {
		pkgMap[p] = true
	}

	for _, p := range fullConfig.Packages {
		if pkgMap[p.Name] {
			filtered.Packages = append(filtered.Packages, p)
		}
	}
	for _, c := range fullConfig.Channels {
		if pkgMap[c.Package] {
			filtered.Channels = append(filtered.Channels, c)
		}
	}
	for _, b := range fullConfig.Bundles {
		if pkgMap[b.Package] {
			filtered.Bundles = append(filtered.Bundles, b)
		}
	}

	return filtered, nil
}
