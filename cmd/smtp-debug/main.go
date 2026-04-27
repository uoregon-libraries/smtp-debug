package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/mail"
	"os"
	"path/filepath"
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

// writeMessage writes a human-readable rendering of the captured message to
// outdir. It returns the path written.
func writeMessage(outdir string, sess *session, received time.Time) (string, error) {
	var stamp = received.Format("2006-01-02_15-04-05")
	var name = fmt.Sprintf("%s_conn%d_msg%d.txt", stamp, sess.connID, sess.msgSeq)
	var path = filepath.Join(outdir, name)

	var raw = strings.Join(sess.dataLines, "\r\n")
	if len(sess.dataLines) > 0 {
		raw += "\r\n"
	}

	var buf strings.Builder
	fmt.Fprintln(&buf, "=== Envelope ===")
	fmt.Fprintf(&buf, "Received:   %s\n", received.Format(time.RFC3339))
	fmt.Fprintf(&buf, "Connection: %d from %s\n", sess.connID, sess.clientAddr)
	fmt.Fprintf(&buf, "MAIL FROM:  %s\n", sess.mailFrom)
	if len(sess.rcptTos) == 0 {
		fmt.Fprintln(&buf, "RCPT TO:    (none)")
	} else {
		fmt.Fprintf(&buf, "RCPT TO:    %s\n", strings.Join(sess.rcptTos, ", "))
	}
	buf.WriteString("\n")

	var msg, parseErr = mail.ReadMessage(strings.NewReader(raw))
	if parseErr != nil {
		fmt.Fprintln(&buf, "=== Raw message (header parse failed) ===")
		fmt.Fprintln(&buf, parseErr.Error())
		buf.WriteString("\n")
		buf.WriteString(raw)
	} else {
		writeHeaders(&buf, msg.Header)
		buf.WriteString("\n=== Body ===\n")
		var body, _ = io.ReadAll(msg.Body)
		buf.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			buf.WriteString("\n")
		}
	}

	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeHeaders prints common headers first in a readable order, then any
// remaining headers, so users can scan the important ones at a glance.
func writeHeaders(buf *strings.Builder, h mail.Header) {
	// Keys must match net/textproto canonicalization (e.g. "Message-Id" not "Message-ID").
	var primary = []string{"From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Date", "Message-Id"}
	var seen = make(map[string]bool, len(primary))

	fmt.Fprintln(buf, "=== Headers ===")
	for _, k := range primary {
		seen[k] = true
		for _, v := range h[k] {
			fmt.Fprintf(buf, "%s: %s\n", k, v)
		}
	}

	var rest []string
	for k := range h {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	if len(rest) > 0 {
		buf.WriteString("\n--- Other headers ---\n")
		for _, k := range rest {
			for _, v := range h[k] {
				fmt.Fprintf(buf, "%s: %s\n", k, v)
			}
		}
	}
}
