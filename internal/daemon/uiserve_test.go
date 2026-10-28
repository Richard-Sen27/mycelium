package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUISignInCode(t *testing.T) {
	env := startLocal(t, nil)
	if code, _ := env.c.doWith("", "POST", "/api/local/ui-code", ""); code != http.StatusUnauthorized {
		t.Fatalf("ui-code without token: %d", code)
	}
	code, body := env.c.do("POST", "/api/local/ui-code", "")
	var issued struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &issued) != nil || len(issued.Code) < 40 || issued.ExpiresIn != 60 {
		t.Fatalf("ui-code: %d %s", code, body)
	}
	if strings.Contains(string(body), env.c.token) {
		t.Fatal("the code response carries the token")
	}
	exchange := func(c string) (int, []byte) {
		return env.c.doWith("", "POST", "/api/local/ui-session", `{"code":"`+c+`"}`)
	}
	code, body = exchange(issued.Code)
	var sess struct {
		Token string `json:"token"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &sess) != nil || sess.Token != env.c.token {
		t.Fatalf("ui-session: %d %s", code, body)
	}
	if code, _ := exchange(issued.Code); code != http.StatusNotFound {
		t.Fatalf("second use: %d", code)
	}
	if code, _ := exchange("nope"); code != http.StatusNotFound {
		t.Fatalf("unknown code: %d", code)
	}
	if code, _ := env.c.doWith("", "POST", "/api/local/ui-session", "not json"); code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", code)
	}
	// At most 8 outstanding codes.
	for i := range maxUICodes {
		if code, _ := env.c.do("POST", "/api/local/ui-code", ""); code != http.StatusOK {
			t.Fatalf("code %d: %d", i, code)
		}
	}
	if code, _ := env.c.do("POST", "/api/local/ui-code", ""); code != http.StatusTooManyRequests {
		t.Fatalf("ninth code: %d", code)
	}
}

func TestUICodeExpiry(t *testing.T) {
	now := time.Now()
	var s uiCodeStore
	s.now = func() time.Time { return now }
	c, err := s.issue()
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(uiCodeTTL)
	if s.redeem(c) {
		t.Fatal("expired code redeemed")
	}
	for range maxUICodes {
		if _, err := s.issue(); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(uiCodeTTL + time.Second)
	if _, err := s.issue(); err != nil {
		t.Fatalf("expired codes still counted: %v", err)
	}
}
