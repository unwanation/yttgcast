// Command yttgcast-auth is the one-time interactive login. It asks for the
// SMS/app code (and the 2FA password if enabled), stores the MTProto session
// in TG_SESSION_FILE and prints it as a base64 string for TG_SESSION_STRING.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"

	"github.com/unwanation/yttgcast/internal/config"
	tgclient "github.com/unwanation/yttgcast/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.TelegramConfigured() {
		return errors.New("TG_API_ID and TG_API_HASH must be set (get them at https://my.telegram.org/apps)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := tgclient.New(tgclient.Options{APIID: cfg.APIID, APIHash: cfg.APIHash, SessionFile: cfg.SessionFile})
	fmt.Printf("Telegram login. Session will be saved to %s\n", cfg.SessionFile)
	flow := auth.NewFlow(&terminalAuth{phone: cfg.Phone}, auth.SendCodeOptions{})
	user, err := client.Login(ctx, func(ctx context.Context, c *telegram.Client) error {
		return c.Auth().IfNecessary(ctx, flow)
	})
	if err != nil {
		return err
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	fmt.Printf("\nLogged in as %s (@%s, id %d).\n", name, user.Username, user.ID)
	fmt.Printf("Session saved to %s - the daemon will pick it up within a minute.\n\n", cfg.SessionFile)

	s, err := client.SessionString()
	if err != nil {
		return err
	}
	fmt.Println("Optional: to run elsewhere without the session file, set in .env:")
	fmt.Println("TG_SESSION_STRING=" + s)
	fmt.Println("\nKeep this secret: it grants full access to the Telegram account.")
	return nil
}

// terminalAuth implements auth.UserAuthenticator with terminal prompts.
type terminalAuth struct{ phone string }

func (t *terminalAuth) Phone(_ context.Context) (string, error) {
	if t.phone != "" {
		fmt.Println("Phone number (from TG_PHONE):", t.phone)
		return t.phone, nil
	}
	return prompt("Phone number (international format, e.g. +491701234567): ")
}

func (t *terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Two-step verification (2FA) password: ")
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(b), err
	}
	return readLine()
}

func (t *terminalAuth) Code(_ context.Context, sent *tg.AuthSentCode) (string, error) {
	hint := "Telegram app"
	switch sent.Type.(type) {
	case *tg.AuthSentCodeTypeSMS:
		hint = "SMS"
	case *tg.AuthSentCodeTypeCall:
		hint = "phone call"
	case *tg.AuthSentCodeTypeEmailCode:
		hint = "email"
	}
	return prompt(fmt.Sprintf("Login code (sent via %s): ", hint))
}

func (t *terminalAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Println("Telegram terms of service:\n" + tos.Text)
	return nil
}

func (t *terminalAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("this phone number has no Telegram account; sign up in the app first")
}

func prompt(msg string) (string, error) {
	fmt.Print(msg)
	return readLine()
}

var stdin = bufio.NewReader(os.Stdin)

func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
