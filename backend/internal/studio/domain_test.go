package studio

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCharacterBudgetExcludesOnlyWhitelistedVariables(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
		bad  bool
	}{
		{"Merhaba {{first_name}}!", 9, false}, {strings.Repeat("ş", 160) + "{{ first_name }}", 160, false}, {"{{email}}", 0, true}, {"{{first_name", 0, true}, {"👋", 1, false},
	} {
		n, e := Budget(tc.text)
		if (e != nil) != tc.bad || (!tc.bad && n != tc.want) {
			t.Fatalf("Budget(%q)=%d,%v", tc.text, n, e)
		}
	}
}
func TestPersonalizationDoesNotConsumeLiteralBudget(t *testing.T) {
	c := Content{SMS: strings.Repeat("a", 160) + "{{first_name}}"}
	ch := SMSChannel{}
	msg, e := ch.Render(c, Recipient{FirstName: strings.Repeat("ş", 90), Address: "fake"})
	if e != nil {
		t.Fatal(e)
	}
	if len([]rune(msg.Body)) != 250 {
		t.Fatal("name should be excluded from literal budget")
	}
	c.SMS += "a"
	if ch.Validate(c) == nil {
		t.Fatal("161 literal characters accepted")
	}
}
func TestTemplateAndRecipientValidation(t *testing.T) {
	if (SMSChannel{}).Validate(Content{SMS: "{{secret}}"}) == nil {
		t.Fatal("unknown variable accepted")
	}
	if (EmailChannel{}).Validate(Content{EmailSubject: "hello\nBcc: someone", EmailBody: "x"}) == nil {
		t.Fatal("header injection accepted")
	}
	if _, e := (SMSChannel{}).Render(Content{SMS: "hello {{first_name}}"}, Recipient{FirstName: "bad\r\nname"}); e == nil {
		t.Fatal("unsafe recipient accepted")
	}
	msg, e := (SMSChannel{}).Render(Content{SMS: "hello {{first_name}}"}, Recipient{})
	if e != nil || msg.Body != "hello arkadaşım" {
		t.Fatal("fallback missing")
	}
}
func TestStructuredLLMValidation(t *testing.T) {
	for _, raw := range []string{`not json`, `{"segment":{"city":"İstanbul","days":30,"exclude_purchased":true},"content":{},"sql":"DROP TABLE users"}`, `{} {}`} {
		if _, e := DecodeProposal([]byte(raw)); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	p := Proposal{Segment: Segment{"İstanbul", 30, true}, Content: Content{"hello", "subject", "body"}}
	b, _ := json.Marshal(p)
	decoded, e := DecodeProposal(b)
	if e != nil {
		t.Fatal(e)
	}
	registry := Registry{Channels: map[string]Channel{"sms": SMSChannel{}, "email": EmailChannel{}}}
	if e = registry.Validate(decoded); e != nil {
		t.Fatal(e)
	}
	p.Segment.City = "İstanbul'; DROP TABLE users;--"
	if registry.Validate(p) == nil {
		t.Fatal("unsupported segment accepted")
	}
	p.Segment = Segment{"İstanbul", 366, true}
	if registry.Validate(p) == nil {
		t.Fatal("unbounded dates")
	}
}
func TestMockLLMStreaming(t *testing.T) {
	var streamed strings.Builder
	p, e := (MockLLM{}).Generate(context.Background(), "Ankara son 7 gün %20 indirim", func(field, delta string) error {
		if field == "sms" {
			streamed.WriteString(delta)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if p.Segment.City != "Ankara" || p.Segment.Days != 7 || !strings.Contains(p.Content.SMS, "%20") || streamed.String() != p.Content.SMS {
		t.Fatal("mock extraction or stream mismatch")
	}
}
func TestPartialJSONStreaming(t *testing.T) {
	raw := `{"content":{"sms":"Merhaba \u0130pek, \"selam\"` // no closing quote yet
	if got := partialField(raw, "sms"); got != "Merhaba İpek, \"selam\"" {
		t.Fatalf("partial=%q", got)
	}
}
func TestRetryClassification(t *testing.T) {
	delay := 3 * time.Second
	if got := RetryDelay(1, &DeferredError{After: delay}, nil); got != delay {
		t.Fatal(got)
	}
	if IsFailure(&DeferredError{After: delay}) || IsFailure(&ProviderError{Kind: Throttled}) {
		t.Fatal("wait should not consume retry budget")
	}
	if !IsFailure(errors.New("network")) {
		t.Fatal("network failure must consume retry budget")
	}
	for i := 0; i < 8; i++ {
		d := RetryDelay(i, errors.New("network"), nil)
		max := time.Second * time.Duration(1<<i)
		if d < max/2 || d > max {
			t.Fatal("invalid jitter")
		}
	}
}

func TestBriefSegmentExplicitConstraints(t *testing.T) {
	for _, tc := range []struct {
		brief   string
		segment Segment
		bad     bool
	}{
		{"İstanbul son 30 gün", Segment{"Ankara", 30, true}, true},
		{"ISTANBUL son 30 gün", Segment{"İstanbul", 30, true}, false},
		{"istanbul son 7 gün", Segment{"İstanbul", 30, true}, true},
		{"İstanbul ve Ankara", Segment{"Ankara", 30, true}, true},
		{"Ankara'daki kullanıcılar son 7 gün", Segment{"Ankara", 7, true}, false},
		{"genel kampanya", Segment{"İstanbul", 30, true}, false},
	} {
		if err := ValidateBriefSegment(tc.brief, tc.segment); (err != nil) != tc.bad {
			t.Fatalf("%s: %v", tc.brief, err)
		}
	}
}
