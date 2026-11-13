package notifications

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const smtpTimeout = 10 * time.Second

// Sender delivers one email.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

type smtpSender struct {
	addr string
	from string
}

// NewSMTPSender returns a Sender that talks plain SMTP, without authentication, to addr.
func NewSMTPSender(addr, from string) Sender {
	return &smtpSender{addr: addr, from: from}
}

// headerSafe removes line breaks so a value cannot inject extra headers.
func headerSafe(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func (s *smtpSender) Send(ctx context.Context, to, subject, body string) error {
	d := net.Dialer{Timeout: smtpTimeout}
	conn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", s.addr, err)
	}
	// One deadline bounds the whole exchange; smtp.Client has no context support.
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	host, _, _ := net.SplitHostPort(s.addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	msg := "From: " + headerSafe(s.from) + "\r\n" +
		"To: " + headerSafe(to) + "\r\n" +
		"Subject: " + headerSafe(subject) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		body + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of data: %w", err)
	}
	return c.Quit()
}
