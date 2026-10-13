package auth

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny SMTP server (no STARTTLS) that records one message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	rcpt []string
	data string
	done chan struct{}
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, done: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(f.done)
		r := bufio.NewReader(conn)
		w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		w("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250-fake")
				w("250 8BITMIME")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				f.mu.Lock()
				f.from = strings.TrimSpace(line[len("MAIL FROM:"):])
				f.mu.Unlock()
				w("250 ok")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				f.mu.Lock()
				f.rcpt = append(f.rcpt, strings.TrimSpace(line[len("RCPT TO:"):]))
				f.mu.Unlock()
				w("250 ok")
			case cmd == "DATA":
				w("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				f.mu.Lock()
				f.data = b.String()
				f.mu.Unlock()
				w("250 queued")
			case cmd == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return f
}

func (f *fakeSMTP) hostPort() (string, int) {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

func newTestMailer(t *testing.T, f *fakeSMTP, requireTLS bool) *SMTPMailer {
	t.Helper()
	host, port := f.hostPort()
	m, err := NewSMTPMailer(SMTPConfig{Host: host, Port: port, From: "StreamAfrica <no-reply@streamafrica.test>", RequireTLS: requireTLS, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSMTPMailer_Send(t *testing.T) {
	f := startFakeSMTP(t)
	m := newTestMailer(t, f, false)
	err := m.Send(context.Background(), Message{To: "ada@example.com", Subject: "Confirm your email — Ọláolú", Body: "Hello Ọláolú\nhttps://x.test/verify#token=abc\n"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(f.from, "<no-reply@streamafrica.test>") || len(f.rcpt) != 1 || f.rcpt[0] != "<ada@example.com>" {
		t.Fatalf("envelope: from=%q rcpt=%v", f.from, f.rcpt)
	}
	for _, want := range []string{
		"From: \"StreamAfrica\" <no-reply@streamafrica.test>", "To: ada@example.com", "MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: quoted-printable",
		"Subject: =?utf-8?q?", "https://x.test/verify#token=3Dabc",
	} {
		if want == "https://x.test/verify#token=3Dabc" {
			want = "verify#token=3Dabc" // QP encodes "=" as =3D
		}
		if !strings.Contains(f.data, want) {
			t.Errorf("message missing %q\n%s", want, f.data)
		}
	}
}

func TestSMTPMailer_RejectsInjection(t *testing.T) {
	f := startFakeSMTP(t)
	m := newTestMailer(t, f, false)
	bad := []Message{
		{To: "ada@example.com\r\nBcc: evil@example.com", Subject: "s", Body: "b"},
		{To: "ada@example.com", Subject: "hi\r\nBcc: evil@example.com", Body: "b"},
		{To: "Ada <ada@example.com>", Subject: "s", Body: "b"},
		{To: "", Subject: "s", Body: "b"},
	}
	for _, msg := range bad {
		if err := m.Send(context.Background(), msg); err == nil {
			t.Errorf("accepted %+v", msg)
		}
	}
}

func TestSMTPMailer_RequireTLSRefusesPlaintextServer(t *testing.T) {
	f := startFakeSMTP(t)
	m := newTestMailer(t, f, true)
	err := m.Send(context.Background(), Message{To: "ada@example.com", Subject: "s", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err=%v", err)
	}
}

func TestSMTPMailer_NoCredentialsOverPlaintext(t *testing.T) {
	f := startFakeSMTP(t)
	host, port := f.hostPort()
	m, _ := NewSMTPMailer(SMTPConfig{Host: host, Port: port, From: "no-reply@streamafrica.test", Username: "u", Password: "p", Timeout: 3 * time.Second})
	err := m.Send(context.Background(), Message{To: "ada@example.com", Subject: "s", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("err=%v", err)
	}
}

func TestNewSMTPMailer_Validation(t *testing.T) {
	if _, err := NewSMTPMailer(SMTPConfig{Host: "h", Port: 25, From: "not an address"}); err == nil {
		t.Error("bad from accepted")
	}
	if _, err := NewSMTPMailer(SMTPConfig{Host: "", Port: 25, From: "a@b.co"}); err == nil {
		t.Error("empty host accepted")
	}
}
