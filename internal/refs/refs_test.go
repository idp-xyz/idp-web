package refs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	keyA = []byte(strings.Repeat("a", 32))
	keyB = []byte(strings.Repeat("b", 32))
)

const (
	alice = "t1/alice"
	page  = "https://example.com/a"
)

func keys(t *testing.T, ks ...[]byte) *Keys {
	t.Helper()
	k, err := New(ks...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestARefIs25CharactersAndIsGoodForItsAddressAndCaller(t *testing.T) {
	k := keys(t, keyA)
	ref := k.Make(alice, page)
	if len(ref) != 25 || ref[8] != '.' {
		t.Fatalf("%q", ref)
	}
	if err := k.Check(alice, page, ref); err != nil {
		t.Fatal(err)
	}
	// white space round a ref copied by a model is not what the check is for
	if err := k.Check(alice, page, " "+ref+"\n"); err != nil {
		t.Fatal(err)
	}
}

func TestARefAlteredInAnyOneCharacterIsRefused(t *testing.T) {
	k := keys(t, keyA)
	ref := k.Make(alice, page)
	for i := range ref {
		for _, c := range "0a2z" {
			if byte(c) == ref[i] {
				continue
			}
			bad := ref[:i] + string(c) + ref[i+1:]
			if err := k.Check(alice, page, bad); err == nil {
				t.Fatalf("%q (altered at %d) was accepted", bad, i)
			}
		}
	}
}

func TestARefMadeForAnotherAddressOrAnotherCallerIsRefused(t *testing.T) {
	k := keys(t, keyA)
	ref := k.Make(alice, page)
	if err := k.Check(alice, page, ref); err != nil {
		t.Fatal(err) // the good case first, so that the refusals below are about the one thing that changed
	}
	if err := k.Check(alice, page+"?d=secret", ref); !errors.Is(err, ErrNotForIt) {
		t.Errorf("another address: %v", err)
	}
	if err := k.Check("t1/bob", page, ref); !errors.Is(err, ErrNotForIt) {
		t.Errorf("another caller: %v", err)
	}
	if err := k.Check("t2/alice", page, ref); !errors.Is(err, ErrNotForIt) {
		t.Errorf("the same subject in another tenant: %v", err)
	}
}

func TestAnExpiredRefIsRefusedAsExpiredAndOnlyWhenItIsOtherwiseGenuine(t *testing.T) {
	k := keys(t, keyA)
	now := time.Unix(1_800_000_000, 0)
	k.now = func() time.Time { return now }
	ref := k.Make(alice, page)
	now = now.Add(Lifetime - time.Second)
	if err := k.Check(alice, page, ref); err != nil {
		t.Fatalf("just before the end: %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := k.Check(alice, page, ref); !errors.Is(err, ErrExpired) {
		t.Fatalf("after the end: %v", err)
	}
	// moving the expiry forward in the text does not revive it: the tag covers the expiry
	forged := "ffffffff" + ref[8:]
	if err := k.Check(alice, page, forged); !errors.Is(err, ErrNotForIt) {
		t.Fatalf("a forged expiry: %v", err)
	}
}

func TestWhatIsNotARefIsMalformed(t *testing.T) {
	k := keys(t, keyA)
	for _, s := range []string{"", "x", strings.Repeat("a", 25), "0000000g.aaaaaaaaaaaaaaaa", "00000000.AAAAAAAAAAAAAAAA", "00000000.aaaaaaaaaaaaaaa1", "00000000.aaaaaaaaaaaaaaaaa", "00000000-aaaaaaaaaaaaaaaa"} {
		if err := k.Check(alice, page, s); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestThePartsAreJoinedWithoutAmbiguity(t *testing.T) {
	k := keys(t, keyA)
	// joined by plain concatenation, or by a separator an address may contain, each pair below would be one message
	for _, c := range []struct{ callerA, addrA, callerB, addrB string }{
		{"bc", "a", "c", "ab"},
		{"c", "a\x00b", "b\x00c", "a"},
	} {
		ref := k.Make(c.callerA, c.addrA)
		if err := k.Check(c.callerA, c.addrA, ref); err != nil {
			t.Fatal(err)
		}
		if err := k.Check(c.callerB, c.addrB, ref); err == nil {
			t.Errorf("(%q, %q) and (%q, %q) share a ref", c.callerA, c.addrA, c.callerB, c.addrB)
		}
	}
}

func TestAfterARotationTheOldKeysStillVerifyAndOnlyTheFirstSigns(t *testing.T) {
	old := keys(t, keyA).Make(alice, page)
	rotated := keys(t, keyB, keyA)
	if err := rotated.Check(alice, page, old); err != nil {
		t.Fatalf("a ref of the old key: %v", err)
	}
	fresh := rotated.Make(alice, page)
	if err := keys(t, keyB).Check(alice, page, fresh); err != nil {
		t.Fatalf("the new ref is signed by the first key: %v", err)
	}
	if err := keys(t, keyA).Check(alice, page, fresh); err == nil {
		t.Fatal("the old key alone accepts a ref of the new one")
	}
	if err := keys(t, keyB).Check(alice, page, old); err == nil {
		t.Fatal("a key that was never configured accepts the old key's ref")
	}
}

func TestKeysShorterThan32BytesAreRefused(t *testing.T) {
	if _, err := New([]byte(strings.Repeat("k", 31))); err == nil {
		t.Error("31 bytes were accepted")
	}
	if _, err := New(keyA, []byte("short")); err == nil {
		t.Error("a short second key was accepted")
	}
	if _, err := New(); err == nil {
		t.Error("no key was accepted")
	}
}

func TestKeysAreReadOneALineFromAFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	k, err := Load(write("ok", string(keyB)+"\r\n\n"+string(keyA)+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(k.keys) != 2 || string(k.keys[0]) != string(keyB) {
		t.Fatalf("%d keys", len(k.keys))
	}
	if _, err := Load(write("short", string(keyA)+"\nshort\n")); err == nil {
		t.Error("a short key in the file was accepted")
	}
	if _, err := Load(write("empty", "\n\n")); err == nil {
		t.Error("an empty file was accepted")
	}
	if _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}
