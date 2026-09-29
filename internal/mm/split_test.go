package mm

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	if got := Split("  hi  ", 100); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("short: %q", got)
	}
	if got := Split("   ", 100); len(got) != 0 {
		t.Fatalf("blank: %q", got)
	}
	text := strings.Repeat("word ", 100) + "\n\n" + strings.Repeat("y", 300)
	parts := Split(text, 200)
	for _, p := range parts {
		if n := len([]rune(p)); n > 200 || n == 0 {
			t.Fatalf("bad chunk size %d", n)
		}
	}
	if strings.Join(strings.Fields(strings.Join(parts, " ")), "") != strings.Join(strings.Fields(text), "") {
		t.Fatal("content lost")
	}
	// multibyte text must not be cut inside a rune
	for _, p := range Split(strings.Repeat("ж", 1000), 300) {
		if strings.ContainsRune(p, '�') {
			t.Fatal("broken rune")
		}
	}
}

func TestParsePostedCarriesChannelName(t *testing.T) {
	ev, ok := parsePosted([]byte(`{"event":"posted","data":{"channel_type":"O","channel_name":"town-square","mentions":"[\"u1\"]","post":"{\"id\":\"p\",\"channel_id\":\"c\",\"user_id\":\"u\",\"message\":\"hi\"}"}}`))
	if !ok || ev.ChannelName != "town-square" || ev.ChannelType != "O" || ev.Post.ChannelID != "c" || len(ev.Mentions) != 1 {
		t.Fatalf("got %+v ok=%v", ev, ok)
	}
}
