package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/uranix/claude-mattermost-bridge/internal/config"
)

// waitCall returns the first call containing text, consuming calls one at a time.
func waitCall(t *testing.T, calls chan string, text string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case c := <-calls:
			seen = append(seen, c)
			if strings.Contains(c, text) {
				return c
			}
		case <-deadline:
			t.Fatalf("no call containing %q; saw %q", text, seen)
		}
	}
}

// noCall fails if a call containing text shows up within a short while.
func noCall(t *testing.T, calls chan string, text string) {
	t.Helper()
	deadline := time.After(700 * time.Millisecond)
	for {
		select {
		case c := <-calls:
			if strings.Contains(c, text) {
				t.Fatalf("unexpected call: %s", c)
			}
		case <-deadline:
			return
		}
	}
}

func startAccess(t *testing.T, mutate func(*config.Config), permApp bool) (*fakeMM, chan string) {
	calls := make(chan string, 64)
	appURL := ""
	if permApp {
		appURL = newFakePermApp(t, calls).URL
	} else {
		appURL = newFakeApp(t, calls).URL
	}
	fm, msrv := newFakeMM(t)
	cfg := &config.Config{
		MattermostURL: msrv.URL, Token: "tok", AllowedUsers: map[string]bool{"alice": true},
		AllowedChannels: map[string]bool{"chan": true}, PermissionPrompts: permApp,
		AppServerURL:   "ws" + strings.TrimPrefix(appURL, "http"),
		PermissionMode: "acceptEdits", AttachDir: t.TempDir(),
	}
	if mutate != nil {
		mutate(cfg)
	}
	stop := startBridge(t, cfg, msrv)
	t.Cleanup(stop)
	return fm, calls
}

func TestChannelOpenToEveryone(t *testing.T) {
	fm, calls := startAccess(t, nil, false)

	// A user who is not on the allowlist can talk to the bot in a channel.
	fm.sayAs("mallory", "O", "chan", "p1", "", "@claude hi there")
	fm.waitPost(t, "hello from claude")
	if c := waitCall(t, calls, "turn/start"); !strings.Contains(c, "@mallory: hi there") {
		t.Fatalf("the prompt should carry the author: %s", c)
	}
	// Follow-ups in the thread need no mention.
	fm.sayAs("mallory", "O", "chan", "p2", "p1", "and another thing")
	waitCall(t, calls, "and another thing")

	// Several people share the thread, with their names on their messages.
	fm.say("O", "chan", "p3", "p1", "alice here too")
	if c := waitCall(t, calls, "alice here too"); !strings.Contains(c, "@Alice: ") {
		t.Fatalf("missing author label: %s", c)
	}
}

func TestDMsStayAllowlistOnly(t *testing.T) {
	fm, calls := startAccess(t, nil, false)
	fm.sayAs("mallory", "D", "dmchan", "p1", "", "let me in")
	noCall(t, calls, "let me in")
	fm.say("D", "dmchan", "p2", "", "hello")
	waitCall(t, calls, "hello")
}

func TestUserAllowlistDoesNotApplyInChannels(t *testing.T) {
	// The DM allowlist names alice only; mallory is served in the allowed channel
	// but not in a DM, and alice is not privileged over mallory in the channel.
	fm, calls := startAccess(t, nil, false)
	fm.sayAs("mallory", "O", "chan", "p1", "", "@claude channel is open")
	waitCall(t, calls, "channel is open")
	fm.sayAs("mallory", "D", "dmchan", "p2", "", "but the DM is not")
	noCall(t, calls, "but the DM is not")
}

func TestOtherBotsAreIgnored(t *testing.T) {
	fm, calls := startAccess(t, nil, false)
	fm.sayAs("botty", "O", "chan", "p1", "", "@claude beep")
	noCall(t, calls, "beep")
}

func TestChannelsNeedToBeAllowlisted(t *testing.T) {
	fm, calls := startAccess(t, nil, false)

	// A channel that is not on the list is ignored, even for an allowlisted user.
	fm.say("O", "other", "p1", "", "@claude are you there")
	fm.sayAs("mallory", "O", "other", "p2", "", "@claude and you")
	noCall(t, calls, "are you there")
	noCall(t, calls, "and you")

	// The listed one works.
	fm.sayAs("mallory", "O", "chan", "p3", "", "@claude hello")
	waitCall(t, calls, "hello")
}

func TestChannelCanBeAllowlistedByName(t *testing.T) {
	fm, calls := startAccess(t, func(c *config.Config) {
		c.AllowedChannels = map[string]bool{"name-townsquare": true} // the fake names channels "name-<id>"
	}, false)
	fm.sayAs("mallory", "O", "townsquare", "p1", "", "@claude by name")
	waitCall(t, calls, "by name")
	fm.sayAs("mallory", "O", "elsewhere", "p2", "", "@claude other channel")
	noCall(t, calls, "other channel")
}

func TestEmptyChannelListMeansNoChannels(t *testing.T) {
	fm, calls := startAccess(t, func(c *config.Config) { c.AllowedChannels = map[string]bool{} }, false)
	fm.say("O", "chan", "p1", "", "@claude anyone?")
	noCall(t, calls, "anyone?")
	fm.say("D", "dmchan", "p2", "", "DMs still work")
	waitCall(t, calls, "DMs still work")
}

func TestEveryoneInAChannelIsEqual(t *testing.T) {
	fm, calls := startAccess(t, nil, true)
	fm.sayAs("mallory", "O", "chan", "p1", "", "@claude list /tmp")
	prompt := fm.waitPost(t, "Permission requested")
	id := prompt["id"].(string)

	// Anyone in the channel can approve, not just the allowlist.
	fm.react("mallory", id, "white_check_mark")
	if c := waitCall(t, calls, "permission/respond"); !strings.Contains(c, `"behavior":"allow"`) {
		t.Fatalf("a channel member's approval must count: %s", c)
	}
	fm.waitPost(t, "ran it")

	// And anyone can change settings: mode, trust and the rest.
	fm.sayAs("mallory", "O", "chan", "p2", "p1", "!trust bash")
	fm.waitPost(t, "Run without asking in this conversation: `Bash`")
	fm.sayAs("mallory", "O", "chan", "p3", "p1", "!mode plan")
	fm.waitPost(t, "Permission mode: `plan`")
}

func TestChannelModeApplies(t *testing.T) {
	fm, calls := startAccess(t, func(c *config.Config) { c.ChannelMode = "default" }, false)
	fm.sayAs("mallory", "O", "chan", "p1", "", "@claude hi")
	if c := waitCall(t, calls, "thread/start"); !strings.Contains(c, `"permission_mode":"default"`) {
		t.Fatalf("channels should start in the channel mode: %s", c)
	}
	// DMs keep the normal mode.
	fm.say("D", "dmchan", "p2", "", "hi")
	if c := waitCall(t, calls, "thread/start"); !strings.Contains(c, `"permission_mode":"acceptEdits"`) {
		t.Fatalf("DMs should keep the normal mode: %s", c)
	}
}
