package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("operating system random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

func MatchKeyword(text, keyword string) bool {
	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if strings.EqualFold(word, keyword) {
			return true
		}
	}
	return false
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,199}$`)

func ValidIdentifier(s string) bool { return identifier.MatchString(s) }

func ValidateAutomation(a AutomationInput) error {
	if !ValidIdentifier(a.AccountID) || a.Platform != "instagram" {
		return fmt.Errorf("%w: valid account_id and instagram platform required", ErrInvalid)
	}
	if n := utf8.RuneCountInString(a.Keyword); n == 0 || n > 64 {
		return fmt.Errorf("%w: keyword must contain 1-64 letters or numbers", ErrInvalid)
	}
	for _, r := range a.Keyword {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return fmt.Errorf("%w: keyword must be a single token of letters or numbers", ErrInvalid)
		}
	}
	for _, s := range []string{a.CommentReply, a.EmailPrompt} {
		if !validText(s, 2000) || strings.TrimSpace(s) == "" {
			return fmt.Errorf("%w: reply and prompt must contain 1-2000 bytes", ErrInvalid)
		}
	}
	u, err := url.Parse(a.Link)
	if err != nil || u.Scheme != "https" || !validLinkHost(u) || !validLinkPort(u) || u.User != nil || len(a.Link) > 2048 || !validText(a.Link, 2048) {
		return fmt.Errorf("%w: link must be an HTTPS URL without credentials", ErrInvalid)
	}
	return nil
}

func validLinkPort(u *url.URL) bool {
	if !strings.Contains(u.Host, ":") || strings.HasSuffix(u.Host, "]") {
		return true
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port >= 1 && port <= 65535
}

func validLinkHost(u *url.URL) bool {
	host := u.Hostname()
	ip, err := netip.ParseAddr(host)
	if strings.HasPrefix(u.Host, "[") {
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	if err == nil {
		return ip.Is4()
	}

	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func ValidateEvent(kind string, e Event) error {
	if kind != "comment" && kind != "message" {
		return fmt.Errorf("%w: unknown event kind", ErrInvalid)
	}
	if e.Platform != "instagram" || !ValidIdentifier(e.EventID) || !ValidIdentifier(e.AccountID) || !ValidIdentifier(e.UserID) || !validText(e.Text, 8000) {
		return fmt.Errorf("%w: event identity, platform, or text", ErrInvalid)
	}
	if kind == "comment" && (!ValidIdentifier(e.PostID) || !ValidIdentifier(e.CommentID) || e.MessageID != "") {
		return fmt.Errorf("%w: comment requires post_id/comment_id and no message_id", ErrInvalid)
	}
	if kind == "message" && (!ValidIdentifier(e.MessageID) || e.PostID != "" || e.CommentID != "") {
		return fmt.Errorf("%w: message requires message_id and no post_id/comment_id", ErrInvalid)
	}
	return nil
}

func validText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return false
		}
	}
	return true
}

// ExtractEmail accepts one distinct unquoted ASCII mailbox with a DNS-style
// domain. This is syntax checking, not proof that the user owns the mailbox.
func ExtractEmail(text string) (string, bool) {
	var found string
	// Keep quoted constructs intact: splitting them can turn a fragment of a
	// quoted local part into an unrelated, apparently valid mailbox.
	quoted := false
	start := 0
	var tokens []string
	for i, r := range text {
		if r == '"' {
			quoted = !quoted
			continue
		}
		if !quoted && (unicode.IsSpace(r) || strings.ContainsRune("<>()[]{},;:“”", r)) {
			if start < i {
				tokens = append(tokens, text[start:i])
			}
			start = i + utf8.RuneLen(r)
		}
	}
	if start < len(text) {
		tokens = append(tokens, text[start:])
	}
	for _, token := range tokens {
		if strings.ContainsRune(token, '"') {
			continue
		}
		// Sentence punctuation can follow the domain; leading punctuation may
		// belong to the local part and must not silently change the mailbox.
		token = strings.TrimRight(token, ".!?")
		if !strings.Contains(token, "@") {
			continue
		}
		if len(token) > 254 {
			continue
		}
		m, err := mail.ParseAddress(token)
		if err != nil || m.Address != token {
			continue
		}
		local, host, ok := strings.Cut(token, "@")
		if !ok || len(local) == 0 || len(local) > 64 || strings.ContainsAny(local, "\"()\\") || !validDomain(host) {
			continue
		}
		ascii := true
		for _, r := range local {
			if r > 127 {
				ascii = false
			}
		}
		if !ascii {
			continue
		}
		if found != "" && found != token {
			return "", false
		}
		found = token
	}
	return found, found != ""
}

func validDomain(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) < 2 || len(host) > 253 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 63 || p[0] == '-' || p[len(p)-1] == '-' {
			return false
		}
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := parts[len(parts)-1]
	if len(tld) < 2 {
		return false
	}
	for _, r := range tld {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func (s State) Active() bool { return s == InitialPending || s == WaitingEmail || s == FinalPending }

// Advance is the ordered acknowledgment transition, independent of SQL/HTTP.
// Capturing an email during InitialPending does not advance delivery order.
func Advance(state State, kind ActionKind, hasEmail bool) (State, *ActionKind, error) {
	next := func(s State, k ActionKind) (State, *ActionKind, error) { return s, &k, nil }
	switch {
	case state == InitialPending && kind == ReplyComment:
		return next(InitialPending, RequestEmail)
	case state == InitialPending && kind == RequestEmail && hasEmail:
		return next(FinalPending, SendLink)
	case state == InitialPending && kind == RequestEmail:
		return WaitingEmail, nil, nil
	case state == FinalPending && kind == SendLink && hasEmail:
		return Completed, nil, nil
	default:
		return state, nil, fmt.Errorf("invalid delivery transition: %s/%s", state, kind)
	}
}
