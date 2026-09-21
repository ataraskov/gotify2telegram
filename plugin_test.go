package main

import (
    "encoding/json"
    "strings"
    "testing"
    "unicode/utf8"
)

// A message is only parsed as Markdown when Gotify says it is one. Everything
// else goes out verbatim, so underscores in links and @usernames survive.
func TestParseModeFollowsContentType(t *testing.T) {
    tests := []struct {
        name string
        json string
        want string
    }{
        {"no extras at all", `{"id":1,"appid":2,"title":"t","message":"m"}`, ""},
        {"empty extras", `{"title":"t","message":"m","extras":{}}`, ""},
        {"explicit text/plain", `{"title":"t","message":"m","extras":{"client::display":{"contentType":"text/plain"}}}`, ""},
        {"unrelated extras only", `{"title":"t","message":"m","extras":{"client::notification":{"click":{"url":"x"}}}}`, ""},
        {"markdown", `{"title":"t","message":"m","extras":{"client::display":{"contentType":"text/markdown"}}}`, "Markdown"},
        {"markdown, odd spelling", `{"title":"t","message":"m","extras":{"client::display":{"contentType":" Text/Markdown "}}}`, "Markdown"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            msg := &GotifyMessage{}
            if err := json.Unmarshal([]byte(tt.json), msg); err != nil {
                t.Fatalf("unmarshal: %v", err)
            }
            if got := msg.parse_mode(); got != tt.want {
                t.Errorf("parse_mode() = %q, want %q", got, tt.want)
            }
        })
    }
}

// The regression this fixes: a plain-text ad carrying a t.me link and an
// @username used to be sent with parse_mode=Markdown, and Telegram ate both
// underscores as an italic span. Two underscores is the dangerous case — they
// pair up into valid Markdown, so nothing errors and the text is mangled
// silently.
func TestPlainAdIsNotParsedAsMarkdown(t *testing.T) {
    raw := `{"appid":3,"title":"ps5","message":"Keywords: ps5\nLink: https://t.me/some_chat/1711789\n\nЦена 6000 din\nСвязь @some_user","extras":{"client::display":{"contentType":"text/plain"}}}`

    msg := &GotifyMessage{}
    if err := json.Unmarshal([]byte(raw), msg); err != nil {
        t.Fatalf("unmarshal: %v", err)
    }
    if got := msg.parse_mode(); got != "" {
        t.Fatalf("parse_mode() = %q, want %q so Telegram sends it verbatim", got, "")
    }

    forwarded := msg.Title + "\n\n" + msg.Message
    for _, want := range []string{"t.me/some_chat/1711789", "@some_user"} {
        if !strings.Contains(forwarded, want) {
            t.Errorf("forwarded text lost %q: %q", want, forwarded)
        }
    }
}

func TestSplitMessageKeepsRunesIntact(t *testing.T) {
    // Cyrillic is two bytes per rune: byte slicing would cut one in half.
    body := strings.Repeat("Цена 6000 din Связь ", 500)

    chunks := split_message(body, telegramMessageLimit)
    if len(chunks) < 2 {
        t.Fatalf("expected the body to be split, got %d chunk(s)", len(chunks))
    }

    for i, chunk := range chunks {
        if !utf8.ValidString(chunk) {
            t.Errorf("chunk %d is not valid UTF-8", i)
        }
        if n := utf8.RuneCountInString(chunk); n > telegramMessageLimit {
            t.Errorf("chunk %d has %d runes, over the %d limit", i, n, telegramMessageLimit)
        }
    }
    if joined := strings.Join(chunks, ""); joined != body {
        t.Errorf("rejoined chunks differ from the original (%d vs %d bytes)", len(joined), len(body))
    }
}

func TestSplitMessageEdgeCases(t *testing.T) {
    if got := split_message("", telegramMessageLimit); got != nil {
        t.Errorf("empty message should produce no chunks, got %q", got)
    }
    if got := split_message("short", telegramMessageLimit); len(got) != 1 || got[0] != "short" {
        t.Errorf("short message should pass through as one chunk, got %q", got)
    }
    // Exactly at the limit is one chunk, one rune over is two.
    exact := strings.Repeat("я", telegramMessageLimit)
    if got := split_message(exact, telegramMessageLimit); len(got) != 1 {
        t.Errorf("message of exactly the limit should be 1 chunk, got %d", len(got))
    }
    if got := split_message(exact+"я", telegramMessageLimit); len(got) != 2 {
        t.Errorf("message one rune over the limit should be 2 chunks, got %d", len(got))
    }
}

func TestSplitCaptionKeepsRunesIntact(t *testing.T) {
    short := "Цена 6000 din"
    if caption, remaining := split_caption(short); caption != short || remaining != "" {
        t.Errorf("short text should fit in the caption, got (%q, %q)", caption, remaining)
    }

    long := strings.Repeat("я", telegramCaptionLimit+50)
    caption, remaining := split_caption(long)
    if !utf8.ValidString(caption) || !utf8.ValidString(remaining) {
        t.Error("caption split produced invalid UTF-8")
    }
    if n := utf8.RuneCountInString(caption); n != telegramCaptionLimit {
        t.Errorf("caption has %d runes, want %d", n, telegramCaptionLimit)
    }
    if caption+remaining != long {
        t.Error("caption and remainder do not reassemble into the original text")
    }
}
