package config

import "testing"

func TestCanonicalAndUpstreamModel(t *testing.T) {
	rename := map[string]map[string]string{"sensenova": {"deepseek-flash": "deepseek-v4.1-flash-1017"}}
	if got := CanonicalModel(rename, "deepseek-flash"); got != "deepseek-v4.1-flash-1017" {
		t.Fatalf("canonical = %q", got)
	}
	if got := CanonicalModel(rename, "other"); got != "other" {
		t.Fatalf("unmapped canonical = %q", got)
	}
	if got := UpstreamModel(rename, "sensenova", "deepseek-v4.1-flash-1017"); got != "deepseek-flash" {
		t.Fatalf("upstream = %q", got)
	}
	if got := UpstreamModel(rename, "tokenplan", "deepseek-v4.1-flash-1017"); got != "deepseek-v4.1-flash-1017" {
		t.Fatalf("native upstream = %q", got)
	}
	if got := UpstreamModel(nil, "sensenova", "m"); got != "m" {
		t.Fatalf("nil upstream = %q", got)
	}
}

func TestNormalizeModelRename(t *testing.T) {
	got, err := NormalizeModelRename(map[string]map[string]string{" sensenova ": {" deepseek-flash ": " deepseek-v4.1-flash-1017 ", "same": "same"}})
	if err != nil {
		t.Fatal(err)
	}
	if got["sensenova"]["deepseek-flash"] != "deepseek-v4.1-flash-1017" || len(got["sensenova"]) != 1 {
		t.Fatalf("normalized rename = %v", got)
	}
	if _, err := NormalizeModelRename(map[string]map[string]string{"a": {"x": "y"}, "b": {"x": "z"}}); err == nil {
		t.Fatal("conflicting rename targets accepted")
	}
	if _, err := NormalizeModelRename(map[string]map[string]string{"a": {"x": "y", "y": "z"}}); err == nil {
		t.Fatal("chained rename accepted")
	}
	if _, err := NormalizeModelRename(map[string]map[string]string{"a": {"": "y"}}); err == nil {
		t.Fatal("empty rename source accepted")
	}
	if got, err := NormalizeModelRename(map[string]map[string]string{"a": {"x": "x"}}); err != nil || got != nil {
		t.Fatalf("identity rename should normalize to nil: %v %v", got, err)
	}
}
