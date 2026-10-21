package uicanary

import "testing"

func TestMaskEmail(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"operator@example.com", "o***r@example.com"},
		{"canary@superserve.ai", "c***y@superserve.ai"},
		{"ab@example.com", "***@example.com"},
		{"a@example.com", "***@example.com"},
		{"not-an-email", "***"},
		{"test.user@company.org", "t***r@company.org"},
	}

	for _, tt := range tests {
		got := maskEmail(tt.input)
		if got != tt.want {
			t.Errorf("maskEmail(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
