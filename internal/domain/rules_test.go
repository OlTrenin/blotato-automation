package domain

import (
	"strings"
	"testing"
)

func TestKeywordMatchesWholeToken(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"pricing", true}, {"Please send PRICING!", true}, {"prepricing", false},
		{"pricing2", false}, {"price", false}, {"💬 pricing?", true}, {"épricing", false},
	} {
		if got := MatchKeyword(tc.text, "pricing"); got != tc.want {
			t.Errorf("%q: got %v", tc.text, got)
		}
	}
}

func TestExtractOneEmailFromProse(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"My email is hello@example.com", "hello@example.com"},
		{"Reach <first.last+pricing@example.co.uk>.", "first.last+pricing@example.co.uk"},
		{"hello@example.com!", "hello@example.com"},
		{"!hello@example.com", "!hello@example.com"},
		{"?hello@example.com", "?hello@example.com"},
		{"Reach !hello@example.com?", "!hello@example.com"},
		{".hello@example.com", ""},
		{"no email here", ""}, {"a@localhost", ""}, {"a@-bad.com", ""},
		{"bad..local@example.com", ""}, {"a@@example.com", ""},
		{"a@example.com and b@example.com", ""},
		{strings.Repeat("a", 65) + "@example.com", ""},
		{"你好@example.com", ""}, {"a@example.c", ""}, {"a@example.123", ""},
		{"hello@example.com hello@example.com", "hello@example.com"},
		{`"alice@example.com"@example.org`, ""},
		{`"a b@example.com"@example.org`, ""},
		{`"alice"@example.org`, ""},
		{`He said "hello there", email alice@example.org`, "alice@example.org"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := ExtractEmail(tc.text)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q,%v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestOrderedDeliveryTransitions(t *testing.T) {
	for _, tc := range []struct {
		state   State
		kind    ActionKind
		email   bool
		want    State
		next    ActionKind
		invalid bool
	}{
		{InitialPending, ReplyComment, false, InitialPending, RequestEmail, false},
		{InitialPending, RequestEmail, false, WaitingEmail, "", false},
		{InitialPending, RequestEmail, true, FinalPending, SendLink, false},
		{FinalPending, SendLink, true, Completed, "", false},
		{InitialPending, SendLink, true, InitialPending, "", true},
		{WaitingEmail, SendLink, true, WaitingEmail, "", true},
		{FinalPending, SendLink, false, FinalPending, "", true},
		{Completed, SendLink, true, Completed, "", true},
		{Failed, RequestEmail, false, Failed, "", true},
		{Expired, ReplyComment, false, Expired, "", true},
	} {
		t.Run(string(tc.state)+"/"+string(tc.kind)+"/"+string(tc.want), func(t *testing.T) {
			state, next, err := Advance(tc.state, tc.kind, tc.email)
			if (err != nil) != tc.invalid || state != tc.want {
				t.Fatalf("got %s err=%v", state, err)
			}
			if tc.next == "" && next != nil || tc.next != "" && (next == nil || *next != tc.next) {
				t.Fatalf("unexpected next action %v", next)
			}
		})
	}
}

func TestValidateAutomation(t *testing.T) {
	base := AutomationInput{AccountID: "account-123", Platform: "instagram", Keyword: "pricing", CommentReply: "Sent you a DM!", EmailPrompt: "What is your email?", Link: "https://example.com/pricing"}
	if err := ValidateAutomation(base); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*AutomationInput){
		func(a *AutomationInput) { a.Platform = "unknown" }, func(a *AutomationInput) { a.Keyword = "two words" },
		func(a *AutomationInput) { a.Link = "http://example.com" }, func(a *AutomationInput) { a.Link = "https://name:secret@example.com" },
		func(a *AutomationInput) { a.CommentReply = " " }, func(a *AutomationInput) { a.EmailPrompt = "null\x00" },
		func(a *AutomationInput) { a.AccountID = "bad\naccount" },
		func(a *AutomationInput) { a.Link = "https://." },
		func(a *AutomationInput) { a.Link = "https://example.com:99999" },
		func(a *AutomationInput) { a.Link = "https://-bad.example" },
		func(a *AutomationInput) { a.Link = "https://example..com" },
		func(a *AutomationInput) { a.Link = "https://[not-an-ip]" },
		func(a *AutomationInput) { a.Link = "https://[127.0.0.1]" },
	} {
		a := base
		change(&a)
		if err := ValidateAutomation(a); err == nil {
			t.Fatalf("accepted invalid config: %+v", a)
		}
	}
}

func TestValidateEventIdentityAndKind(t *testing.T) {
	e := Event{EventID: "evt", Platform: "instagram", AccountID: "account", PostID: "post", CommentID: "comment", UserID: "user", Text: "pricing"}
	if err := ValidateEvent("comment", e); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEvent("message", e); err == nil {
		t.Fatal("comment accepted as message")
	}
	e.MessageID = "message"
	if err := ValidateEvent("comment", e); err == nil {
		t.Fatal("mixed event accepted")
	}
	e.CommentID = ""
	e.PostID = ""
	if err := ValidateEvent("message", e); err != nil {
		t.Fatal(err)
	}
	e.Text = strings.Repeat("a", 8001)
	if err := ValidateEvent("message", e); err == nil {
		t.Fatal("oversized text accepted")
	}
}

func TestValidLinkDestinations(t *testing.T) {
	for _, link := range []string{"https://example.com:65535/path", "https://localhost:443", "https://127.0.0.1", "https://[::1]:443/path", "https://example.com./", "https://xn--bcher-kva.example"} {
		a := AutomationInput{AccountID: "a", Platform: "instagram", Keyword: "k", CommentReply: "r", EmailPrompt: "p", Link: link}
		if err := ValidateAutomation(a); err != nil {
			t.Errorf("valid URL %q: %v", link, err)
		}
	}
}
