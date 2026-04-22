/*
Copyright 2026 Marius Bertram.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// OperatorEntrySignature returns a stable, content-derived hash that uniquely
// identifies one ImageSet operator-catalog spec entry for caching purposes.
//
// Two entries that resolve to the same upstream image set MUST produce the
// same signature, regardless of slice ordering inside the spec. The hash
// covers: catalog ref, full-mode flag, skip-dependencies flag, and the sorted
// package list including their channel/bundle filters.
//
// The result is the hex SHA-256 digest, suitable for use as the suffix in
// CatalogDigestAnnotationPrefix annotation keys (annotations have a
// 63-character value-prefix limit on label-style domain segments, but
// annotation values/keys are not so constrained beyond DNS-subdomain rules;
// using the full hex hash is still safe and well below limits).
func OperatorEntrySignature(op Operator) string {
	type pkgSig struct {
		Name           string           `json:"n"`
		DefaultChannel string           `json:"d,omitempty"`
		Channels       []IncludeChannel `json:"c,omitempty"`
		IncludeBundle  IncludeBundle    `json:"b,omitempty"`
	}
	pkgs := make([]pkgSig, 0, len(op.Packages))
	for _, p := range op.Packages {
		ch := append([]IncludeChannel(nil), p.Channels...)
		sort.Slice(ch, func(i, j int) bool { return ch[i].Name < ch[j].Name })
		pkgs = append(pkgs, pkgSig{
			Name:           p.Name,
			DefaultChannel: p.DefaultChannel,
			Channels:       ch,
			IncludeBundle:  p.IncludeBundle,
		})
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })

	payload := struct {
		Catalog          string   `json:"cat"`
		TargetCatalog    string   `json:"tc,omitempty"`
		TargetTag        string   `json:"tt,omitempty"`
		Full             bool     `json:"f,omitempty"`
		SkipDependencies bool     `json:"sd,omitempty"`
		Packages         []pkgSig `json:"p,omitempty"`
	}{
		Catalog:          op.Catalog,
		TargetCatalog:    op.TargetCatalog,
		TargetTag:        op.TargetTag,
		Full:             op.Full,
		SkipDependencies: op.SkipDependencies,
		Packages:         pkgs,
	}
	return hashJSON(payload)
}

// ReleaseChannelSignature returns a stable, content-derived hash that uniquely
// identifies one ImageSet release-channel spec entry for caching purposes.
//
// Two entries that resolve to the same upstream payload MUST produce the same
// signature. The hash covers: channel name, type, min/max bounds, Full and
// ShortestPath flags, the sorted architecture list, and the KubeVirt flag
// (since that toggles which payload-extracted images are required).
func ReleaseChannelSignature(rc ReleaseChannel, architectures []string, kubeVirt bool) string {
	arch := append([]string(nil), architectures...)
	sort.Strings(arch)
	payload := struct {
		Name         string       `json:"n"`
		Type         PlatformType `json:"t,omitempty"`
		MinVersion   string       `json:"min,omitempty"`
		MaxVersion   string       `json:"max,omitempty"`
		Full         bool         `json:"f,omitempty"`
		ShortestPath bool         `json:"sp,omitempty"`
		Arch         []string     `json:"a,omitempty"`
		KubeVirt     bool         `json:"kv,omitempty"`
	}{
		Name:         rc.Name,
		Type:         rc.Type,
		MinVersion:   rc.MinVersion,
		MaxVersion:   rc.MaxVersion,
		Full:         rc.Full,
		ShortestPath: rc.ShortestPath,
		Arch:         arch,
		KubeVirt:     kubeVirt,
	}
	return hashJSON(payload)
}

// CatalogDigestAnnotationKey returns the full annotation key for a given
// operator-entry signature. Use OperatorEntrySignature() to produce sig.
// Kubernetes annotation name parts must be ≤63 chars; since the prefix
// "catalog-digest-" is 15 chars, the sig is truncated to 48 chars.
func CatalogDigestAnnotationKey(sig string) string {
	if len(sig) > 48 {
		sig = sig[:48]
	}
	return CatalogDigestAnnotationPrefix + sig
}

// ReleaseDigestAnnotationKey returns the full annotation key for a given
// release-channel signature. Use ReleaseChannelSignature() to produce sig.
// Kubernetes annotation name parts must be ≤63 chars; since the prefix
// "release-digest-" is 15 chars, the sig is truncated to 48 chars.
func ReleaseDigestAnnotationKey(sig string) string {
	if len(sig) > 48 {
		sig = sig[:48]
	}
	return ReleaseDigestAnnotationPrefix + sig
}

func hashJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Should never happen for the structs we feed in; fall back to a
		// deterministic-but-debuggable string so callers still get a usable key.
		b = []byte(fmt.Sprintf("%v", v))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
