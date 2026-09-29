package bridge

import "testing"

var testModels = []modelInfo{
	{Value: "default", ResolvedModel: "claude-sonnet-5-5", DisplayName: "Default (recommended)"},
	{Value: "sonnet", ResolvedModel: "claude-sonnet-5-5", DisplayName: "Sonnet"},
	{Value: "sonnet[1m]", ResolvedModel: "claude-sonnet-5-5[1m]", DisplayName: "Sonnet 5.5 (1M context)"},
	{Value: "claude-fable-5-1", ResolvedModel: "claude-fable-5-1", DisplayName: "Fable"},
	{Value: "opus", ResolvedModel: "claude-opus-5-5", DisplayName: "Opus"},
	{Value: "opus[1m]", ResolvedModel: "claude-opus-5-5[1m]", DisplayName: "Opus (1M context)"},
	{Value: "haiku", ResolvedModel: "claude-haiku-4-5-20251001", DisplayName: "Haiku"},
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
		n    int // candidates when ambiguous
	}{
		{"opus", "opus", true, 0},            // exact beats the partial match on opus[1m]
		{"OPUS", "opus", true, 0},            // case-insensitive
		{"fab", "claude-fable-5-1", true, 0}, // unique partial name
		{"hai", "haiku", true, 0},
		{"claude-opus-5-5", "opus", true, 0}, // exact resolved id
		{"default", "default", true, 0},
		{"sonnet[1m]", "sonnet[1m]", true, 0},
		{"1m", "", false, 2},                            // ambiguous: lists candidates
		{"claude-opus-9-9", "claude-opus-9-9", true, 0}, // explicit id not in the list
		{"gpt", "", false, 0},                           // unknown
		{"--evil", "", false, 0},
	}
	for _, tc := range cases {
		v, cands, ok := resolveModel(tc.in, testModels)
		if ok != tc.ok || v != tc.want || len(cands) != tc.n {
			t.Errorf("%q: got (%q, %d candidates, %v), want (%q, %d, %v)", tc.in, v, len(cands), ok, tc.want, tc.n, tc.ok)
		}
	}
}

func TestResolveModelWithoutList(t *testing.T) {
	if v, _, ok := resolveModel("claude-opus-5-5", nil); !ok || v != "claude-opus-5-5" {
		t.Fatalf("explicit id should work without a list: %q %v", v, ok)
	}
	if _, _, ok := resolveModel("opus", nil); ok {
		t.Fatal("aliases need the list")
	}
}
