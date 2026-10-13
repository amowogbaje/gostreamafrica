package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Message is a plain-text email.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Mailer sends email. The SMTP version talks to Mailpit locally and a real provider elsewhere.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// SMTPConfig configures SMTPMailer.
type SMTPConfig struct {
	Host       string
	Port       int
	From       string // "Name <addr@example.com>" or a bare address
	Username   string
	Password   string
	RequireTLS bool // refuse to send unless STARTTLS succeeds
	Timeout    time.Duration
}

// SMTPMailer sends mail with net/smtp. It upgrades to TLS when the server offers STARTTLS
// and never sends credentials over an unencrypted connection.
type SMTPMailer struct {
	cfg  SMTPConfig
	from *mail.Address
}

// NewSMTPMailer validates the config.
func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, errors.New("invalid SMTP_FROM")
	}
	if cfg.Host == "" || cfg.Port < 1 {
		return nil, errors.New("invalid SMTP host or port")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SMTPMailer{cfg: cfg, from: from}, nil
}

// Send delivers one message.
func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	to, err := mail.ParseAddress(msg.To)
	if err != nil || to.Address != msg.To || strings.ContainsAny(msg.To, "\r\n") {
		return errors.New("invalid recipient")
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return errors.New("invalid subject")
	}
	data, err := m.build(to, msg)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := net.Dialer{Timeout: m.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(m.cfg.Timeout))
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = c.Close() }()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	} else if m.cfg.RequireTLS {
		return errors.New("smtp server does not offer STARTTLS and TLS is required")
	}
	if m.cfg.Username != "" {
		if _, encrypted := c.TLSConnectionState(); !encrypted {
			return errors.New("refusing to send SMTP credentials without TLS")
		}
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}

func (m *SMTPMailer) build(to *mail.Address, msg Message) ([]byte, error) {
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	domain := "streamafrica.local"
	if at := strings.LastIndex(m.from.Address, "@"); at >= 0 {
		domain = m.from.Address[at+1:]
	}
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", m.from.String())
	h("To", to.Address)
	h("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	h("Date", time.Now().UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(msg.Body)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
