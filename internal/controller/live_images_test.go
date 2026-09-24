package controller

import "testing"

func TestRepoAndDigest(t *testing.T) {
	const hex = "1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct{ dest, repo, digest string }{
		{"reg.io/a/b@sha256:" + hex, "reg.io/a/b", "sha256:" + hex},
		{"reg.io/a/b:sha256-" + hex, "reg.io/a/b", "sha256:" + hex},
		{"reg.io/openshift/release:4.16.11-x86_64-etcd", "reg.io/openshift/release:4.16.11-x86_64-etcd", ""},
		{"reg.io:5000/a:v1", "reg.io:5000/a:v1", ""},
	}
	for _, tt := range tests {
		repo, digest := repoAndDigest(tt.dest)
		if repo != tt.repo || digest != tt.digest {
			t.Errorf("repoAndDigest(%q) = (%q, %q), want (%q, %q)", tt.dest, repo, digest, tt.repo, tt.digest)
		}
	}
}

func TestLiveImagesNeeds(t *testing.T) {
	const hex = "2222222222222222222222222222222222222222222222222222222222222222"
	live := liveImages{
		dests:   map[string]bool{"reg.io/a:v1": true, "reg.io/c:sha256-" + hex: true},
		digests: map[string]bool{"reg.io/c@sha256:" + hex: true},
	}
	tests := map[string]bool{
		"reg.io/a:v1":            true,  // live itself
		"reg.io/b:v1":            false, // plain tag, not live
		"reg.io/c@sha256:" + hex: true,  // same manifest as a live tag
		"reg.io/d@sha256:" + hex: false, // same digest, other repository
		"reg.io/c:sha256-" + hex: true,
		"reg.io/c:other-tag":     false, // tags are deleted individually
	}
	for dest, want := range tests {
		if got := live.needs(dest); got != want {
			t.Errorf("needs(%q) = %v, want %v", dest, got, want)
		}
	}
}
