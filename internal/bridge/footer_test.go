package bridge

import "testing"

func TestFooter(t *testing.T) {
	cases := []struct {
		u    usageInfo
		want string
	}{
		{usageInfo{}, ""},
		{usageInfo{model: "claude-haiku-4-5-20251001"}, "_haiku 4.5_"},
		{usageInfo{model: "claude-opus-5-5", effort: "high", window: 1000000, context: 45210, in: 1234567, out: 38000},
			"_opus 5.5 | high | ctx 45.2k/1M | tokens 1.2M in, 38.0k out_"},
		{usageInfo{model: "some-model", context: 950}, "_some-model | ctx 950_"},
	}
	for _, c := range cases {
		if got := c.u.footer(); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.u, got, c.want)
		}
	}
}
