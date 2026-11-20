package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/jackdzi/ledger-proxy/ledger"
)

func main() {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, ".config", "ledger"), "ledgerd's config folder")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	path := filepath.Join(dir, "simplefin_access_url")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; delete it first to link again", path)
	}

	fmt.Print("Paste your SimpleFIN setup token and press Enter: ")
	token, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(token) == "" {
		return errors.New("no token given")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	accessURL, err := ledger.ClaimSetupToken(ctx, token)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(accessURL+"\n"), 0o600); err != nil {
		return fmt.Errorf("save access URL: %w (the token is now used up: make a new one)", err)
	}
	fmt.Println("✓ Linked. Access URL saved to", path, "(readable only by you).")

	// List the accounts (one request out of the ~24/day quota).
	sf, err := ledger.NewSimpleFIN(accessURL)
	if err != nil {
		return err
	}
	_, set, err := sf.Fetch(ctx, time.Now().AddDate(0, 0, -1), time.Now())
	if err != nil {
		return err
	}
	for _, m := range set.Messages() {
		fmt.Println("  bank says:", m)
	}
	fmt.Printf("\nYour accounts. Put the lines you want in %s\n", filepath.Join(dir, "bank_accounts.txt"))
	fmt.Println("(use the SAME ledger account as the card in cards.txt, so taps and bank rows can be matched):")
	fmt.Println()
	for _, a := range set.Accounts {
		fmt.Printf("%s = %s   # %s\n", a.ID, suggest(a.Name), a.Name)
	}
	fmt.Println("\nThen: systemctl --user restart ledgerd")
	return nil
}

func suggest(name string) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, strings.Title(strings.ToLower(name)))
	lower := strings.ToLower(name)
	if strings.Contains(lower, "card") || strings.Contains(lower, "credit") {
		return "Liabilities:" + clean
	}
	return "Assets:" + clean
}
