package main

import (
	"strings"
	"testing"
)

func TestAccountKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain lowercase", "fellow", "fellow"},
		{"space becomes a separator", "Fellow Tst1", "fellow_tst1"},
		{"any symbol is a separator", "MIKE-GULAK", "mike_gulak"},
		{"underscore and space are the same", "mike_gulak", "mike_gulak"},
		{"separators at both ends are trimmed", "  mike  ", "mike"},
		{"runs of separators are kept", "a  b", "a__b"},
		{"multi-byte character is one separator per byte", "caéf", "ca__f"},
		{"kelvin sign is not lowercased to k", "Kay", "ay"},
		{"exactly 12 characters", "abcdefghijkl", "abcdefghijkl"},
		{"12 characters after trimming", "  abcdefghijkl  ", "abcdefghijkl"},
		{"13 characters is rejected", "abcdefghijklm", ""},
		{"13 with a separator at 12 is rejected", "abcdefghijk lm", ""},
		{"no letters or digits", "___", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountKey(tt.in); got != tt.want {
				t.Errorf("accountKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// serverVisibleName is what DataConversions.usernameToHash hashes, given
// the bytes the client sends at login: lowercased, non-alphanumerics as
// spaces, trimmed, at most 12 characters. Input is always ASCII here (the
// output of encodeCredential), where Java's toLowerCase and trim() agree
// with these Go calls.
func serverVisibleName(wire string) string {
	b := []byte(strings.ToLower(wire))
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
			b[i] = ' '
		}
	}
	s := strings.Trim(string(b), " ")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// serverUsernameHash ports DataConversions.usernameToHash from the OpenRSC
// server (server/src/com/openrsc/server/util/rsc/DataConversions.java).
func serverUsernameHash(wire string) int64 {
	var l int64
	for _, c := range []byte(serverVisibleName(wire)) {
		l *= 37
		switch {
		case c >= 'a' && c <= 'z':
			l += int64(1+c) - 97
		case c >= '0' && c <= '9':
			l += int64(27+c) - 48
		}
	}
	return l
}

// The server sees a name of 1-12 characters only when, before truncation,
// the trimmed name already fit; accountKey rejects the rest.
func serverAcceptsLength(wire string) bool {
	b := []byte(strings.ToLower(wire))
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
			b[i] = ' '
		}
	}
	n := len(strings.Trim(string(b), " "))
	return n >= 1 && n <= 12
}

// FuzzAccountKeyMatchesServer checks the property the adapter depends on:
// two usernames get the same key exactly when the game server would treat
// their logins as the same account, and a key fed back in is unchanged.
func FuzzAccountKeyMatchesServer(f *testing.F) {
	seeds := [][2]string{
		{"Fellow Tst1", "fellow_tst1"},
		{"a  b", "a b"},
		{"caéf", "ca__f"},
		{"Kay", "ay"},
		{"abcdefghijkl", "ABCDEFGHIJKL"},
		{"abcdefghijk lm", "abcdefghijk"},
		{"-mike-", "mike"},
		{"", "_"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		wireA, wireB := string(encodeCredential(a)), string(encodeCredential(b))
		ka, kb := accountKey(a), accountKey(b)

		for _, c := range []struct{ in, wire, key string }{{a, wireA, ka}, {b, wireB, kb}} {
			if (c.key != "") != serverAcceptsLength(c.wire) {
				t.Fatalf("accountKey(%q) = %q, but server length check says %t", c.in, c.key, serverAcceptsLength(c.wire))
			}
			if c.key != "" && accountKey(c.key) != c.key {
				t.Fatalf("accountKey not idempotent: %q -> %q -> %q", c.in, c.key, accountKey(c.key))
			}
		}
		if ka == "" || kb == "" {
			return
		}

		sameKey := ka == kb
		sameAccount := serverUsernameHash(wireA) == serverUsernameHash(wireB)
		if sameKey != sameAccount {
			t.Fatalf("%q -> %q and %q -> %q: same key %t, but server same account %t", a, ka, b, kb, sameKey, sameAccount)
		}
	})
}

func TestLoginReason(t *testing.T) {
	tests := []struct {
		code   int
		reason string
		fatal  bool
	}{
		{-1, "connection_error", false},
		{3, "invalid_credentials", true},
		{4, "already_logged_in", false},
		{5, "client_outdated", true},
		{7, "login_attempts_exceeded", false},
		{11, "account_temporarily_disabled", false},
		{12, "account_permanently_disabled", true},
		{14, "world_full", false},
		{20, "server_error", false},
		{100, "unexpected_response", false},
		{42, "unknown_response_42", false},
	}
	for _, tt := range tests {
		reason, fatal := loginReason(tt.code)
		if reason != tt.reason || fatal != tt.fatal {
			t.Errorf("loginReason(%d) = (%q, %t), want (%q, %t)", tt.code, reason, fatal, tt.reason, tt.fatal)
		}
	}
}
