package chat

import "testing"

// TestVerifyRoundTrip checks a generated line verifies, and a wrong password
// does not.
func TestVerifyRoundTrip(t *testing.T) {
	line := HashPassword("alice", "correct horse")
	users, err := parseRecords(line)
	if err != nil {
		t.Fatal(err)
	}
	c := &Creds{users: users, dummy: newRecord("x")}
	if !c.Verify("alice", "correct horse") {
		t.Error("right password rejected")
	}
	if c.Verify("alice", "wrong") {
		t.Error("wrong password accepted")
	}
	if c.Verify("bob", "correct horse") {
		t.Error("unknown user accepted")
	}
}

// TestSaltIsRandom guards against a fixed salt making hashes comparable.
func TestSaltIsRandom(t *testing.T) {
	if HashPassword("a", "same") == HashPassword("a", "same") {
		t.Fatal("two hashes of one password must differ")
	}
}

// TestParseRecordRejectsJunk keeps a malformed users file from loading.
func TestParseRecordRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "bcrypt$1$a$b", "pbkdf2-sha256$10$a$b", "pbkdf2-sha256$600000$!!$b"} {
		if _, err := parseRecord(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
