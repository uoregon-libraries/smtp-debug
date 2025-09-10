package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"strings"
)

func main() {
	var port = flag.Int("port", 25, "The port to listen on for SMTP connections")
	flag.Parse()

	var addr = fmt.Sprintf(":%d", *port)
	var listener, err = net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on port %d: %v", *port, err)
	}
	defer listener.Close()

	fmt.Printf("RAW SMTP Debug Server listening on port %d...\n", *port)
	fmt.Println("Logging ALL raw SMTP traffic - no parsing, just pure data")
	fmt.Println("Press Ctrl+C to stop")
	fmt.Println(strings.Repeat("=", 70))

	var connectionCount = 0

	for {
		var conn, err = listener.Accept()
		if err != nil {
			slog.Warn("Failed to accept connection: %v", err)
			continue
		}

		connectionCount++
		go handleRawConnection(conn, connectionCount)
	}
}

func handleRawConnection(conn net.Conn, connID int) {
	defer conn.Close()

	var clientAddr = conn.RemoteAddr().String()
	var logger = slog.With("connID", connID, "clientAddr", clientAddr)
	logger.Info("Connection start")

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

		// Minimal SMTP responses to keep the conversation going
		var trimmedLine = strings.TrimSpace(strings.ToUpper(line))
		var response string

		if strings.HasPrefix(trimmedLine, "HELO ") || strings.HasPrefix(trimmedLine, "EHLO ") {
			if strings.HasPrefix(trimmedLine, "EHLO ") {
				response = "250-raw-debug.local\r\n250-8BITMIME\r\n250 ENHANCEDSTATUSCODES\r\n"
			} else {
				response = "250 raw-debug.local\r\n"
			}
		} else if strings.HasPrefix(trimmedLine, "MAIL FROM:") {
			response = "250 OK\r\n"
		} else if strings.HasPrefix(trimmedLine, "RCPT TO:") {
			response = "250 OK\r\n"
		} else if strings.HasPrefix(trimmedLine, "DATA") {
			response = "354 End data with <CR><LF>.<CR><LF>\r\n"
		} else if trimmedLine == "." {
			response = "250 OK: Message accepted\r\n"
		} else if trimmedLine == "QUIT" {
			response = "221 Bye\r\n"
			logger.Info("server → client", "message", response)
			conn.Write([]byte(response))
			break
		} else if trimmedLine == "RSET" {
			response = "250 OK\r\n"
		} else if trimmedLine == "NOOP" {
			response = "250 OK\r\n"
		} else if trimmedLine == "HELP" {
			response = "214 Raw debug server\r\n"
		} else {
			// For DATA content or unknown commands, just give a generic OK
			// This way we capture everything without breaking the flow
			if len(strings.TrimSpace(line)) > 0 {
				logger.Warn("DATA content or something unknown, sending OK")
				response = "250 OK\r\n"
			}
		}

		if response != "" {
			logger.Info("server → client", "message", response)
			conn.Write([]byte(response))
		}
	}

	logger.Info("Connection closed")
}
