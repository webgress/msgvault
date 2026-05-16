package store

import "testing"

// TestRedactPassword verifies that display-formatting of a database path or
// PG URL removes the password while preserving everything else.
func TestRedactPassword(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "SQLite file path unchanged",
			in:   "/home/user/.msgvault/msgvault.db",
			want: "/home/user/.msgvault/msgvault.db",
		},
		{
			name: ":memory: SQLite unchanged",
			in:   ":memory:",
			want: ":memory:",
		},
		{
			name: "postgres URL with password redacted",
			in:   "postgres://alice:s3cret@host:5432/db",
			want: "postgres://alice:***@host:5432/db",
		},
		{
			name: "postgresql scheme also handled",
			in:   "postgresql://alice:s3cret@host:5432/db",
			want: "postgresql://alice:***@host:5432/db",
		},
		{
			name: "postgres URL without password unchanged",
			in:   "postgres://alice@host/db",
			want: "postgres://alice@host/db",
		},
		{
			name: "postgres URL without userinfo unchanged",
			in:   "postgres://host:5432/db",
			want: "postgres://host:5432/db",
		},
		{
			name: "postgres URL preserves query params",
			in:   "postgres://alice:s3cret@host/db?sslmode=require&search_path=ms",
			want: "postgres://alice:***@host/db?sslmode=require&search_path=ms",
		},
		{
			name: "malformed URL unchanged",
			in:   "postgres://not a valid url",
			want: "postgres://not a valid url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactPassword(tc.in)
			if got != tc.want {
				t.Errorf("RedactPassword(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
