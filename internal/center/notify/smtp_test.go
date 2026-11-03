package notify

import (
	"bufio"
	"io"
	"net"
	"strings"
	"time"
)

func smtpFixture(listener net.Listener, fail string, done chan<- string) {
	conn, err := listener.Accept()
	if err != nil {
		done <- ""
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if fail == "greeting" {
		_, _ = io.WriteString(conn, "500 unavailable\r\n")
		done <- ""
		return
	}
	_, _ = io.WriteString(conn, "220 fixture\r\n")
	reader := bufio.NewReader(conn)
	body := ""
	data := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			done <- body
			return
		}
		if data {
			if line == ".\r\n" {
				data = false
				if fail == "body" {
					_, _ = io.WriteString(conn, "500 body failed\r\n")
				} else {
					_, _ = io.WriteString(conn, "250 accepted\r\n")
				}
				continue
			}
			body += line
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"):
			if fail == "tls" {
				_, _ = io.WriteString(conn, "250-fixture\r\n250 STARTTLS\r\n")
			} else {
				_, _ = io.WriteString(conn, "250-fixture\r\n250 AUTH PLAIN\r\n")
			}
		case strings.HasPrefix(line, "AUTH") || strings.HasPrefix(line, "STARTTLS"):
			_, _ = io.WriteString(conn, "500 rejected\r\n")
		case strings.HasPrefix(line, "MAIL"):
			if fail == "mail" {
				_, _ = io.WriteString(conn, "500 rejected\r\n")
			} else {
				_, _ = io.WriteString(conn, "250 ok\r\n")
			}
		case strings.HasPrefix(line, "RCPT"):
			if fail == "rcpt" {
				_, _ = io.WriteString(conn, "500 rejected\r\n")
			} else {
				_, _ = io.WriteString(conn, "250 ok\r\n")
			}
		case strings.HasPrefix(line, "DATA"):
			if fail == "data" {
				_, _ = io.WriteString(conn, "500 rejected\r\n")
			} else {
				_, _ = io.WriteString(conn, "354 send\r\n")
				data = true
			}
		case strings.HasPrefix(line, "QUIT"):
			if fail == "quit" {
				_, _ = io.WriteString(conn, "500 rejected\r\n")
			} else {
				_, _ = io.WriteString(conn, "221 bye\r\n")
			}
			done <- body
			return
		default:
			_, _ = io.WriteString(conn, "250 ok\r\n")
		}
	}
}
