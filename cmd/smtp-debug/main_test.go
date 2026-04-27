package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractAddr(t *testing.T) {
	var cases = []struct {
		in, want string
	}{
		{"<alice@example.com>", "alice@example.com"},
		{" <alice@example.com>", "alice@example.com"},
		{"<alice@example.com> SIZE=42", "alice@example.com"},
		{"alice@example.com", "alice@example.com"},
		{" alice@example.com SIZE=42", "alice@example.com"},
		{"<>", ""},
	}
	for _, c := range cases {
		var got = extractAddr(c.in)
		if got != c.want {
			t.Errorf("extractAddr(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestWriteMessage(t *testing.T) {
	var dir = t.TempDir()
	var sess = &session{
		connID:     7,
		clientAddr: "10.0.0.5:12345",
		mailFrom:   "alice@example.com",
		rcptTos:    []string{"bob@example.com", "carol@example.com"},
		dataLines: []string{
			"From: Alice <alice@example.com>",
			"To: Bob <bob@example.com>",
			"Subject: Hello",
			"Message-ID: <abc@example.com>",
			"X-Custom: weird",
			"",
			"Body line one.",
			"Body line two.",
		},
		msgSeq: 1,
	}
	var when = time.Date(2026, 4, 27, 13, 5, 5, 0, time.UTC)

	var path, err = writeMessage(dir, sess, when)
	if err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	if got := filepath.Base(path); got != "2026-04-27_13-05-05_conn7_msg1.txt" {
		t.Errorf("filename = %q", got)
	}

	var data, readErr = os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	var got = string(data)

	var wantSubstrings = []string{
		"=== Envelope ===",
		"Connection: 7 from 10.0.0.5:12345",
		"MAIL FROM:  alice@example.com",
		"RCPT TO:    bob@example.com, carol@example.com",
		"=== Headers ===",
		"From: Alice <alice@example.com>",
		"To: Bob <bob@example.com>",
		"Subject: Hello",
		"Message-Id: <abc@example.com>",
		"--- Other headers ---",
		"X-Custom: weird",
		"=== Body ===",
		"Body line one.",
		"Body line two.",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("output missing %q\n--- output ---\n%s", s, got)
		}
	}

	// Headers section should appear before Body section.
	if i, j := strings.Index(got, "=== Headers ==="), strings.Index(got, "=== Body ==="); i < 0 || j < 0 || i > j {
		t.Errorf("header/body order wrong: headers=%d body=%d", i, j)
	}
}

func TestHandleRawConnection_FullSession(t *testing.T) {
	var dir = t.TempDir()
	var serverConn, clientConn = net.Pipe()

	var handlerDone = make(chan struct{})
	go func() {
		handleRawConnection(serverConn, 1, dir)
		close(handlerDone)
	}()

	var responses = make(chan []byte, 1)
	go func() {
		var b, _ = io.ReadAll(clientConn)
		responses <- b
	}()

	var script = strings.Join([]string{
		"EHLO test.local",
		"MAIL FROM:<alice@example.com> SIZE=99",
		"RCPT TO:<bob@example.com>",
		"RCPT TO:<carol@example.com>",
		"DATA",
		"From: Alice <alice@example.com>",
		"To: Bob <bob@example.com>",
		"Subject: First",
		"Message-ID: <first@example.com>",
		"",
		"First body line.",
		"..dot-stuffed line",
		".",
		"RSET",
		"MAIL FROM:<carol@example.com>",
		"RCPT TO:<dave@example.com>",
		"DATA",
		"From: carol@example.com",
		"To: dave@example.com",
		"Subject: Second",
		"",
		"Second body.",
		".",
		"QUIT",
		"",
	}, "\r\n")

	if _, err := io.WriteString(clientConn, script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	// Don't close clientConn here: the handler is still processing the buffered
	// input. It will hit QUIT, return, and its deferred Close on serverConn will
	// EOF the drain goroutine. Closing now races with response delivery.

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit")
	}
	clientConn.Close()

	var resp string
	select {
	case b := <-responses:
		resp = string(b)
	case <-time.After(2 * time.Second):
		t.Fatal("response drain did not finish")
	}

	// Two messages should have been accepted, and we should have said goodbye.
	if got := strings.Count(resp, "250 OK: Message accepted"); got != 2 {
		t.Errorf("expected 2 message-accepted responses, got %d\n--- responses ---\n%s", got, resp)
	}
	if !strings.Contains(resp, "221 Bye") {
		t.Errorf("expected 221 Bye in responses\n--- responses ---\n%s", resp)
	}
	// EHLO multi-line response.
	if !strings.Contains(resp, "250-raw-debug.local") {
		t.Errorf("expected EHLO multi-line greeting in responses\n--- responses ---\n%s", resp)
	}

	// We must NOT have replied to individual DATA-body lines — only the closing dot
	// produces a 250 response, and there should be exactly two of those total
	// (one per accepted message). Per-line OKs would yield far more.
	if got := strings.Count(resp, "250 OK\r\n"); got > 8 {
		t.Errorf("too many 250 OK responses (%d) — likely replying inside DATA mode\n--- responses ---\n%s", got, resp)
	}

	var entries, dirErr = os.ReadDir(dir)
	if dirErr != nil {
		t.Fatalf("read dir: %v", dirErr)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 2 message files, got %d: %v", len(entries), names)
	}

	// Check the first message: dot-stuffing must be undone, envelope captured.
	var first = mustReadFileContaining(t, dir, "Subject: First")
	if !strings.Contains(first, "MAIL FROM:  alice@example.com") {
		t.Errorf("first message envelope wrong:\n%s", first)
	}
	if !strings.Contains(first, "RCPT TO:    bob@example.com, carol@example.com") {
		t.Errorf("first message recipients wrong:\n%s", first)
	}
	if !strings.Contains(first, ".dot-stuffed line") {
		t.Errorf("dot-stuffing not undone:\n%s", first)
	}
	if strings.Contains(first, "..dot-stuffed line") {
		t.Errorf("leading dot still doubled:\n%s", first)
	}

	// Check the second message: envelope must reflect the post-RSET state.
	var second = mustReadFileContaining(t, dir, "Subject: Second")
	if !strings.Contains(second, "MAIL FROM:  carol@example.com") {
		t.Errorf("second message envelope wrong:\n%s", second)
	}
	if !strings.Contains(second, "RCPT TO:    dave@example.com") {
		t.Errorf("second message recipients wrong:\n%s", second)
	}
	if strings.Contains(second, "alice@example.com") || strings.Contains(second, "bob@example.com") {
		t.Errorf("second message leaked first message's envelope:\n%s", second)
	}
}

func mustReadFileContaining(t *testing.T, dir, marker string) string {
	t.Helper()
	var entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		var b, err = os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), marker) {
			return string(b)
		}
	}
	t.Fatalf("no file in %s contains %q", dir, marker)
	return ""
}
