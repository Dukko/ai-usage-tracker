package providers

import (
	"encoding/base64"
	"testing"
)

func TestParsePasted(t *testing.T) {
	cases := []struct{ in, code, state string }{
		{"abc", "abc", ""},
		{"  abc#xyz \n", "abc", "xyz"},
		{"http://localhost:1455/auth/callback?code=c1&scope=openid&state=s1", "c1", "s1"},
	}
	for _, c := range cases {
		code, state := parsePasted(c.in)
		if code != c.code || state != c.state {
			t.Errorf("parsePasted(%q) = %q,%q want %q,%q", c.in, code, state, c.code, c.state)
		}
	}
}

func TestCheckPastedState(t *testing.T) {
	p := PendingAuth{State: "good"}
	if _, err := checkPasted("code#bad", p); err == nil {
		t.Error("expected state mismatch")
	}
	if code, err := checkPasted("code#good", p); err != nil || code != "code" {
		t.Errorf("got %q, %v", code, err)
	}
}

func TestWindowNames(t *testing.T) {
	if got := windowName(18000); got != "5h" {
		t.Errorf("windowName(18000) = %q", got)
	}
	if got := windowName(604800); got != "7d" {
		t.Errorf("windowName(604800) = %q", got)
	}
	if got := claudeWindowName("seven_day_opus"); got != "7d opus" {
		t.Errorf("claudeWindowName = %q", got)
	}
}

func TestChatGPTImport(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"exp":2000000000,"https://api.openai.com/auth":{"chatgpt_account_id":"acct_1","chatgpt_plan_type":"pro"}}`))
	jwt := "h." + payload + ".s"
	c, err := chatgptSub{}.ImportJSON([]byte(`{"tokens":{"id_token":"` + jwt + `","access_token":"` + jwt + `","refresh_token":"r"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountID != "acct_1" || c.Plan != "pro" || c.ExpiresAt != 2000000000 || c.RefreshToken != "r" {
		t.Errorf("unexpected creds: %+v", c)
	}
}

func TestClaudeImport(t *testing.T) {
	c, err := claudeSub{}.ImportJSON([]byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1700000000000,"subscriptionType":"max"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "a" || c.ExpiresAt != 1700000000 || c.Plan != "max" {
		t.Errorf("unexpected creds: %+v", c)
	}
}
