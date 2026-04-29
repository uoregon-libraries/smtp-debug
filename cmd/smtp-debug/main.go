package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type session struct {
	connID     int
	clientAddr string
	mailFrom   string
	rcptTos    []string
	inData     bool
	dataLines  []string
	msgSeq     int
}

func (s *session) resetEnvelope() {
	s.mailFrom = ""
	s.rcptTos = nil
	s.inData = false
	s.dataLines = nil
}

func main() {
	var port = flag.Int("port", 25, "The port to listen on for SMTP connections")
	var outdir = flag.String("out", "", "Path to write out SMTP files (optional)")
	flag.Parse()

	if *outdir != "" {
		if err := os.MkdirAll(*outdir, 0o755); err != nil {
			log.Fatalf("Failed to create output dir %q: %v", *outdir, err)
		}
	}

	var addr = fmt.Sprintf(":%d", *port)
	var listener, err = net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}
	defer listener.Close()

	fmt.Printf("RAW SMTP Debug Server listening on port %d...\n", *port)
	fmt.Println("Logging ALL raw SMTP traffic - no parsing, just pure data")
	if *outdir != "" {
		fmt.Printf("Writing intercepted messages to %s\n", *outdir)
	}
	fmt.Println("Press Ctrl+C to stop")
	fmt.Println(strings.Repeat("=", 70))

	var connectionCount = 0

	for {
		var conn, err = listener.Accept()
		if err != nil {
			slog.Warn("Failed to accept connection", "error", err)
			continue
		}

		connectionCount++
		go handleRawConnection(conn, connectionCount, *outdir)
	}
}

func handleRawConnection(conn net.Conn, connID int, outdir string) {
	defer conn.Close()

	var clientAddr = conn.RemoteAddr().String()
	var logger = slog.With("connID", connID, "clientAddr", clientAddr)
	logger.Info("Connection start")

	var sess = &session{connID: connID, clientAddr: clientAddr}
	var reader = bufio.NewReader(conn)

	// Send initial SMTP greeting
	var greeting = "220 raw-debug.local ESMTP Raw Debug Server\r\n"
	logger.Info("server → client", "message", greeting)
	conn.Write([]byte(greeting))

	for {
		var line, err = reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				logger.Error("Error reading from client", "error", err)
			} else {
				logger.Info("Connection closed by client")
			}
			break
		}
		logger.Info("client → server", "message", line)

		var response string
		var quit bool

		if sess.inData {
			response = handleDataLine(logger, sess, line, outdir)
		} else {
			response, quit = handleCommand(logger, sess, line)
		}

		if response != "" {
			logger.Info("server → client", "message", response)
			conn.Write([]byte(response))
		}
		if quit {
			break
		}
	}

	logger.Info("Connection closed")
}

// handleDataLine accumulates message body lines. SMTP requires that the server
// stay silent during the DATA phase until the terminating "." is seen, so we
// only return a response for the end-of-data marker.
func handleDataLine(logger *slog.Logger, sess *session, line, outdir string) string {
	var content = strings.TrimRight(line, "\r\n")
	if content == "." {
		sess.msgSeq++
		if outdir != "" {
			var path, err = writeMessage(outdir, sess, time.Now())
			if err != nil {
				logger.Error("Failed to write message", "error", err)
			} else {
				logger.Info("Message written", "path", path)
			}
		}
		sess.resetEnvelope()
		return "250 OK: Message accepted\r\n"
	}

	// Undo SMTP dot-stuffing: a leading "." on a content line is escaped.
	content = strings.TrimPrefix(content, ".")
	sess.dataLines = append(sess.dataLines, content)
	return ""
}

// handleCommand processes a non-DATA SMTP command and returns the response and
// whether the connection should close.
func handleCommand(logger *slog.Logger, sess *session, line string) (string, bool) {
	var trimmed = strings.TrimSpace(line)
	var upper = strings.ToUpper(trimmed)

	switch {
	case strings.HasPrefix(upper, "EHLO "):
		return "250-raw-debug.local\r\n250-8BITMIME\r\n250 ENHANCEDSTATUSCODES\r\n", false
	case strings.HasPrefix(upper, "HELO "):
		return "250 raw-debug.local\r\n", false
	case strings.HasPrefix(upper, "MAIL FROM:"):
		sess.mailFrom = extractAddr(trimmed[len("MAIL FROM:"):])
		return "250 OK\r\n", false
	case strings.HasPrefix(upper, "RCPT TO:"):
		sess.rcptTos = append(sess.rcptTos, extractAddr(trimmed[len("RCPT TO:"):]))
		return "250 OK\r\n", false
	case upper == "DATA":
		sess.inData = true
		return "354 End data with <CR><LF>.<CR><LF>\r\n", false
	case upper == "QUIT":
		return "221 Bye\r\n", true
	case upper == "RSET":
		sess.resetEnvelope()
		return "250 OK\r\n", false
	case upper == "NOOP":
		return "250 OK\r\n", false
	case upper == "HELP":
		return "214 Raw debug server\r\n", false
	case trimmed == "":
		return "", false
	default:
		logger.Warn("Unknown command, sending OK")
		return "250 OK\r\n", false
	}
}

// extractAddr pulls the email address out of a "MAIL FROM:" / "RCPT TO:"
// argument, which can look like "<addr>", "<addr> SIZE=123", or just "addr".
func extractAddr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			return s[i+1 : i+j]
		}
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}

// writeMessage writes a semantic HTML rendering of the captured message to
// outdir. It returns the path written.
func writeMessage(outdir string, sess *session, received time.Time) (string, error) {
	var stamp = received.Format("2006-01-02_15-04-05")
	var name = fmt.Sprintf("%s_conn%d_msg%d.html", stamp, sess.connID, sess.msgSeq)
	var path = filepath.Join(outdir, name)

	var raw = strings.Join(sess.dataLines, "\r\n")
	if len(sess.dataLines) > 0 {
		raw += "\r\n"
	}

	var msg, parseErr = mail.ReadMessage(strings.NewReader(raw))

	var title string
	if parseErr != nil {
		title = "(unparsed message) | SMTP Logs"
	} else {
		var dec = new(mime.WordDecoder)
		var decode = func(s string) string {
			if d, err := dec.DecodeHeader(s); err == nil {
				return d
			}
			return s
		}
		var subj = decode(firstOr(msg.Header["Subject"], "(no subject)"))
		var from = decode(firstOr(msg.Header["From"], "(unknown sender)"))
		var to = decode(firstOr(msg.Header["To"], "(unknown recipient)"))
		title = fmt.Sprintf("%s | %s - %s | SMTP Logs", subj, from, to)
	}

	var buf strings.Builder
	writePageStart(&buf, title)
	writeEnvelope(&buf, sess, received)

	if parseErr != nil {
		fmt.Fprintln(&buf, `<section>`)
		fmt.Fprintln(&buf, `<h2>Raw message (header parse failed)</h2>`)
		fmt.Fprintf(&buf, "<p>%s</p>\n", html.EscapeString(parseErr.Error()))
		fmt.Fprintf(&buf, "<pre>%s</pre>\n", html.EscapeString(raw))
		fmt.Fprintln(&buf, `</section>`)
	} else {
		writeHeaders(&buf, msg.Header)
		writeBody(&buf, msg)
	}

	writePageEnd(&buf)

	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func firstOr(vals []string, fallback string) string {
	if len(vals) == 0 {
		return fallback
	}
	return vals[0]
}

func writePageStart(buf *strings.Builder, title string) {
	var esc = html.EscapeString(title)
	fmt.Fprintln(buf, "<!DOCTYPE html>")
	fmt.Fprintln(buf, `<html lang="en">`)
	fmt.Fprintln(buf, "<head>")
	fmt.Fprintln(buf, `<meta charset="utf-8">`)
	fmt.Fprintf(buf, "<title>%s</title>\n", esc)
	fmt.Fprintln(buf, "</head>")
	fmt.Fprintln(buf, "<body>")
	fmt.Fprintf(buf, "<h1>%s</h1>\n", esc)
}

func writePageEnd(buf *strings.Builder) {
	fmt.Fprintln(buf, "</body>")
	fmt.Fprintln(buf, "</html>")
}

func writeEnvelope(buf *strings.Builder, sess *session, received time.Time) {
	fmt.Fprintln(buf, `<section>`)
	fmt.Fprintln(buf, `<h2>Envelope</h2>`)
	fmt.Fprintln(buf, `<dl>`)
	fmt.Fprintf(buf, "<dt>Received</dt><dd>%s</dd>\n", html.EscapeString(received.Format(time.RFC3339)))
	fmt.Fprintf(buf, "<dt>Connection</dt><dd>%d from %s</dd>\n", sess.connID, html.EscapeString(sess.clientAddr))
	fmt.Fprintf(buf, "<dt>MAIL FROM</dt><dd>%s</dd>\n", html.EscapeString(sess.mailFrom))
	if len(sess.rcptTos) == 0 {
		fmt.Fprintln(buf, `<dt>RCPT TO</dt><dd>(none)</dd>`)
	} else {
		fmt.Fprintf(buf, "<dt>RCPT TO</dt><dd>%s</dd>\n", html.EscapeString(strings.Join(sess.rcptTos, ", ")))
	}
	fmt.Fprintln(buf, `</dl>`)
	fmt.Fprintln(buf, `</section>`)
}

// writeHeaders prints common headers first in a readable order, then any
// remaining headers, so users can scan the important ones at a glance.
func writeHeaders(buf *strings.Builder, h mail.Header) {
	// Keys must match net/textproto canonicalization (e.g. "Message-Id" not "Message-ID").
	var primary = []string{"From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Date", "Message-Id"}
	var seen = make(map[string]bool, len(primary))

	fmt.Fprintln(buf, `<section>`)
	fmt.Fprintln(buf, `<h2>Headers</h2>`)
	fmt.Fprintln(buf, `<dl>`)
	for _, k := range primary {
		seen[k] = true
		for _, v := range h[k] {
			fmt.Fprintf(buf, "<dt>%s</dt><dd>%s</dd>\n", html.EscapeString(k), html.EscapeString(v))
		}
	}
	fmt.Fprintln(buf, `</dl>`)
	fmt.Fprintln(buf, `</section>`)

	var rest []string
	for k := range h {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		fmt.Fprintln(buf, `<section>`)
		fmt.Fprintln(buf, `<h2>Other headers</h2>`)
		fmt.Fprintln(buf, `<dl>`)
		for _, k := range rest {
			for _, v := range h[k] {
				fmt.Fprintf(buf, "<dt>%s</dt><dd>%s</dd>\n", html.EscapeString(k), html.EscapeString(v))
			}
		}
		fmt.Fprintln(buf, `</dl>`)
		fmt.Fprintln(buf, `</section>`)
	}
}

// writeBody renders the message body. If it's HTML (directly or via
// multipart/alternative), it's embedded in a sandboxed iframe so the email's
// markup can't collide with the surrounding page. Plain text falls back to a
// <pre> block.
func writeBody(buf *strings.Builder, msg *mail.Message) {
	fmt.Fprintln(buf, `<section>`)
	fmt.Fprintln(buf, `<h2>Body</h2>`)

	var body, _ = io.ReadAll(msg.Body)
	var ct = msg.Header.Get("Content-Type")
	var cte = msg.Header.Get("Content-Transfer-Encoding")
	var mediaType, params, _ = mime.ParseMediaType(ct)

	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		renderMultipart(buf, body, params["boundary"])
	case mediaType == "text/html":
		renderHTML(buf, decodeBody(body, cte))
	default:
		renderText(buf, decodeBody(body, cte))
	}

	fmt.Fprintln(buf, `</section>`)
}

func renderMultipart(buf *strings.Builder, body []byte, boundary string) {
	var htmlPart, textPart []byte
	walkMultipart(body, boundary, &htmlPart, &textPart)
	switch {
	case htmlPart != nil:
		renderHTML(buf, htmlPart)
	case textPart != nil:
		renderText(buf, textPart)
	default:
		renderText(buf, body)
	}
}

func walkMultipart(body []byte, boundary string, htmlPart, textPart *[]byte) {
	if boundary == "" {
		return
	}
	var mr = multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		var part, err = mr.NextPart()
		if err != nil {
			return
		}
		var partBody, _ = io.ReadAll(part)
		var ct = part.Header.Get("Content-Type")
		var cte = part.Header.Get("Content-Transfer-Encoding")
		var mt, params, _ = mime.ParseMediaType(ct)
		switch {
		case strings.HasPrefix(mt, "multipart/"):
			walkMultipart(partBody, params["boundary"], htmlPart, textPart)
		case mt == "text/html" && *htmlPart == nil:
			*htmlPart = decodeBody(partBody, cte)
		case mt == "text/plain" && *textPart == nil:
			*textPart = decodeBody(partBody, cte)
		}
	}
}

func decodeBody(body []byte, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		if d, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body))); err == nil {
			return d
		}
	case "base64":
		if d, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(body))); err == nil {
			return d
		}
	}
	return body
}

func renderHTML(buf *strings.Builder, body []byte) {
	fmt.Fprintf(buf,
		`<iframe sandbox srcdoc="%s" title="Email body" style="width:100%%;min-height:600px;border:1px solid #ccc"></iframe>`+"\n",
		html.EscapeString(string(body)),
	)
}

func renderText(buf *strings.Builder, body []byte) {
	fmt.Fprintf(buf, "<pre>%s</pre>\n", html.EscapeString(string(body)))
}
