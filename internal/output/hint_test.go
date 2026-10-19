package output

import "testing"

// I-38: a hint is its own sentence, even after a message with no closing period.
func TestWithHint(t *testing.T) {
	for _, c := range []struct{ msg, hint, want string }{
		{"you already hold a wallet under this hub", "It is registered here.", "you already hold a wallet under this hub. It is registered here."},
		{"not allowed (sign_message scope)", "`cashctl wallet get-info` lists them.", "not allowed (sign_message scope). `cashctl wallet get-info` lists them."},
		{"already a sentence.", "Next.", "already a sentence. Next."},
		{"question?", "Next.", "question? Next."},
		{"no hint", "", "no hint"},
	} {
		if got := withHint(c.msg, c.hint); got != c.want {
			t.Errorf("withHint(%q, %q) = %q, want %q", c.msg, c.hint, got, c.want)
		}
	}
}
