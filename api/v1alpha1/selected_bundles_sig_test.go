package v1alpha1

import "testing"

func TestOperatorEntrySignature_Bundles(t *testing.T) {
	base := Operator{Catalog: "reg/cat:v1", IncludeConfig: IncludeConfig{Packages: []IncludePackage{{Name: "op"}}}}
	withBundles := func(names ...string) Operator {
		op := base
		pkg := IncludePackage{Name: "op"}
		for _, n := range names {
			pkg.Bundles = append(pkg.Bundles, SelectedBundle{Name: n})
		}
		op.Packages = []IncludePackage{pkg}
		return op
	}

	plain := OperatorEntrySignature(base)
	a := OperatorEntrySignature(withBundles("op.v1", "op.v2"))
	if a == plain {
		t.Fatal("selecting bundles must change the signature")
	}
	if OperatorEntrySignature(withBundles("op.v2", "op.v1")) != a {
		t.Fatal("bundle order must not change the signature")
	}
	if OperatorEntrySignature(withBundles("op.v1")) == a {
		t.Fatal("a different selection must change the signature")
	}
	if OperatorEntrySignature(withBundles()) != plain {
		t.Fatal("an empty selection must keep the signature of existing specs")
	}
}
