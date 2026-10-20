package gateway

import "testing"

func TestPlatformCatalogCoversRegistered(t *testing.T) {
	cat := platformCatalog()
	if len(cat) == 0 {
		t.Fatal("platform catalog must not be empty")
	}
	seen := map[string]bool{}
	prefixes := map[string]bool{}
	for _, p := range cat {
		if p.ID == "" || p.Name == "" || p.BaseURL == "" || p.Credential == "" {
			t.Fatalf("incomplete catalog entry: %+v", p)
		}
		if seen[p.ID] {
			t.Fatalf("duplicate catalog entry %q", p.ID)
		}
		seen[p.ID] = true
		if p.Quota != "snapshot" && p.Quota != "unsupported" {
			t.Fatalf("platform %q has unknown quota mode %q", p.ID, p.Quota)
		}
		// Every known platform must declare an account-id prefix so the console
		// never falls back to a generic slice of the provider id.
		if p.IDPrefix == "" {
			t.Fatalf("platform %q is missing id_prefix", p.ID)
		}
		if prefixes[p.IDPrefix] {
			t.Fatalf("duplicate id_prefix %q", p.IDPrefix)
		}
		prefixes[p.IDPrefix] = true
	}
	for _, want := range []string{"sensenova", "workbuddy", "tokenplan"} {
		if !seen[want] {
			t.Fatalf("catalog is missing registered platform %q", want)
		}
	}
}

func TestPlatformCatalogTokenPlan(t *testing.T) {
	var tp PlatformInfo
	for _, p := range platformCatalog() {
		if p.ID == "tokenplan" {
			tp = p
		}
	}
	if tp.Name != "Intern-ai" {
		t.Fatalf("tokenplan display name = %q, want Intern-ai", tp.Name)
	}
	if tp.BaseURL != "https://discovery-api.intern-ai.org.cn" {
		t.Fatalf("tokenplan base url = %q", tp.BaseURL)
	}
	if tp.Logo != "logo-tokenplan" || tp.Auth != "api_key" || tp.Quota != "unsupported" {
		t.Fatalf("tokenplan metadata = %+v", tp)
	}
	if tp.RequiresUID {
		t.Fatal("tokenplan must not require a uid field")
	}
}
