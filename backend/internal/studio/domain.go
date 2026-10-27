package studio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Segment struct {
	City             string `json:"city"`
	Days             int    `json:"days"`
	ExcludePurchased bool   `json:"exclude_purchased"`
}
type Content struct {
	SMS          string `json:"sms"`
	EmailSubject string `json:"email_subject"`
	EmailBody    string `json:"email_body"`
}
type Proposal struct {
	Segment Segment `json:"segment"`
	Content Content `json:"content"`
}
type Audience struct {
	Total int `json:"total"`
	SMS   int `json:"sms"`
	Email int `json:"email"`
}
type Campaign struct {
	ID            string                    `json:"id"`
	Brief         string                    `json:"brief"`
	Status        string                    `json:"status"`
	Version       int                       `json:"version"`
	Proposal      Proposal                  `json:"proposal"`
	Audience      Audience                  `json:"audience"`
	Error         string                    `json:"error"`
	CreatedAt     time.Time                 `json:"created_at"`
	Counts        map[string]int            `json:"counts"`
	ChannelCounts map[string]map[string]int `json:"channel_counts"`
}
type Recipient struct{ FirstName, Address string }
type Message struct {
	Channel string `json:"channel"`
	Address string `json:"address"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}
type Channel interface {
	Name() string
	Validate(Content) error
	Render(Content, Recipient) (Message, error)
}
type Provider interface {
	Send(context.Context, string, Message) (string, error)
}
type ReceiptLookup interface {
	Lookup(context.Context, string) (string, bool, error)
}

type LLM interface {
	Generate(context.Context, string, func(string, string) error) (Proposal, error)
}
type ErrorKind string

const (
	Transient ErrorKind = "transient"
	Throttled ErrorKind = "throttled"
	Permanent ErrorKind = "permanent"
)

type ProviderError struct {
	Kind       ErrorKind
	RetryAfter time.Duration
	Code       string
	HTTPStatus int
	Message    string
	RequestID  string
}

func (e *ProviderError) Error() string { return string(e.Kind) + ": " + e.Code }

type DeferredError struct{ After time.Duration }

func (e *DeferredError) Error() string { return "deferred" }

func NewID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

var variable = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

func Budget(s string) (int, error) {
	for _, m := range variable.FindAllStringSubmatch(s, -1) {
		if m[1] != "first_name" {
			return 0, fmt.Errorf("Desteklenmeyen değişken: %s", m[1])
		}
	}
	literal := variable.ReplaceAllString(s, "")
	if strings.Contains(literal, "{{") || strings.Contains(literal, "}}") {
		return 0, errors.New("Geçersiz kişiselleştirme değişkeni")
	}
	return utf8.RuneCountInString(literal), nil
}
func validateText(s string, limit int) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("İçerik boş olamaz")
	}
	if len(s) > 40000 {
		return errors.New("İçerik çok uzun")
	}
	n, e := Budget(s)
	if e != nil {
		return e
	}
	if n > limit {
		return fmt.Errorf("İçerik %d / %d karakter; isimler hariç", n, limit)
	}
	for _, r := range s {
		if r < 32 && r != '\n' && r != '\t' {
			return errors.New("Geçersiz kontrol karakteri")
		}
	}
	return nil
}

type SMSChannel struct{}

func (SMSChannel) Name() string             { return "sms" }
func (SMSChannel) Validate(c Content) error { return validateText(c.SMS, 160) }
func (s SMSChannel) Render(c Content, r Recipient) (Message, error) {
	if e := s.Validate(c); e != nil {
		return Message{}, e
	}
	name, e := safeName(r.FirstName)
	if e != nil {
		return Message{}, e
	}
	return Message{Channel: "sms", Address: r.Address, Body: render(c.SMS, name)}, nil
}

type EmailChannel struct{}

func (EmailChannel) Name() string { return "email" }
func (EmailChannel) Validate(c Content) error {
	if strings.ContainsAny(c.EmailSubject, "\r\n") {
		return errors.New("Email konusunda satır sonu olamaz")
	}
	if e := validateText(c.EmailSubject, 80); e != nil {
		return e
	}
	return validateText(c.EmailBody, 10000)
}
func (s EmailChannel) Render(c Content, r Recipient) (Message, error) {
	if e := s.Validate(c); e != nil {
		return Message{}, e
	}
	name, e := safeName(r.FirstName)
	if e != nil {
		return Message{}, e
	}
	return Message{Channel: "email", Address: r.Address, Subject: render(c.EmailSubject, name), Body: render(c.EmailBody, name)}, nil
}
func safeName(name string) (string, error) {
	if name == "" {
		return "arkadaşım", nil
	}
	if utf8.RuneCountInString(name) > 100 {
		return "", errors.New("İsim çok uzun")
	}
	for _, r := range name {
		if r < 32 {
			return "", errors.New("İsim kontrol karakteri içeriyor")
		}
	}
	return name, nil
}
func render(s, name string) string {
	return variable.ReplaceAllStringFunc(s, func(string) string { return name })
}

type Registry struct {
	Channels  map[string]Channel
	Providers map[string]Provider
}

func (r *Registry) Validate(p Proposal) error {
	if p.Segment.Days < 1 || p.Segment.Days > 365 {
		return errors.New("Gün aralığı 1–365 olmalı")
	}
	if !p.Segment.ExcludePurchased {
		return errors.New("Bu segment yalnızca satın alınmamış sepetleri destekler")
	}
	if p.Segment.City != "İstanbul" && p.Segment.City != "Ankara" && p.Segment.City != "İzmir" {
		return errors.New("Desteklenmeyen şehir")
	}
	for _, ch := range r.Channels {
		if e := ch.Validate(p.Content); e != nil {
			return fmt.Errorf("%s: %w", ch.Name(), e)
		}
	}
	return nil
}
