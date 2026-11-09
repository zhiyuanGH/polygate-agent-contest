package buildagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplayLogPersistsAttemptsAcrossAgentRestart(t *testing.T) {
	root := t.TempDir()
	ledger, err := NewReplayLog(root)
	if err != nil {
		t.Fatal(err)
	}
	g := Grant{TaskID: "11111111-1111-4111-8111-111111111111", Attempt: 1,
		AccountID:    "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		SourceSHA256: "sha256:" + strings.Repeat("b", 64), SourceBytes: 100,
		ExpiresUnix: time.Now().Add(time.Minute).Unix()}
	if err = ledger.Claim(g); err != nil {
		t.Fatal(err)
	}
	if err = ledger.Claim(g); !errors.Is(err, ErrGrantReplayed) {
		t.Fatalf("same-process replay: %v", err)
	}
	reopened, err := NewReplayLog(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Claim(g); !errors.Is(err, ErrGrantReplayed) {
		t.Fatalf("restart replay: %v", err)
	}
	g.Attempt = 2
	if err = reopened.Claim(g); err != nil {
		t.Fatalf("new lease attempt: %v", err)
	}
	if err = os.WriteFile(filepath.Join(root, "grant-ledger.log"), []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewReplayLog(root); err == nil {
		t.Fatal("corrupt ledger accepted")
	}
}
