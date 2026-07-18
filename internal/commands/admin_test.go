package commands

import (
	"bytes"
	"strings"
	"testing"
)

// The password is read from stdin so it never lands in shell history or the
// process table. These pin the input handling, which is where a silent
// "the password I set does not work" bug would live: a stray trailing newline
// becomes part of the credential and only surfaces at the next login attempt.
func TestReadPasswordFromStdin(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"trailing newline stripped (echo / heredoc form)", "hunter2-hunter2\n", "hunter2-hunter2"},
		{"no trailing newline (printf form)", "hunter2-hunter2", "hunter2-hunter2"},
		{"CRLF stripped", "hunter2-hunter2\r\n", "hunter2-hunter2"},
		{"only the first line is used", "hunter2-hunter2\nignored\n", "hunter2-hunter2"},
		{"internal spaces preserved", "a long pass phrase\n", "a long pass phrase"},
		{"empty input", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPasswordFromStdin(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("readPasswordFromStdin: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

// A leading/trailing space could be a deliberate part of a passphrase, so it
// must survive. Only the line terminator is stripped.
func TestReadPasswordFromStdin_PreservesLeadingAndTrailingSpaces(t *testing.T) {
	got, err := readPasswordFromStdin(strings.NewReader("  spaced pass  \n"))
	if err != nil {
		t.Fatalf("readPasswordFromStdin: %v", err)
	}
	if got != "  spaced pass  " {
		t.Errorf("got %q; want the spaces preserved", got)
	}
}

func TestRunAdminSetPassword_RequiresUser(t *testing.T) {
	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader("a-password\n"), AdminSetPasswordCmd{})
	if code != 2 {
		t.Fatalf("exit = %d; want 2 for a missing --user", code)
	}
}

func TestRunAdminSetPassword_RequiresPasswordOnStdin(t *testing.T) {
	var out bytes.Buffer
	code := runAdminSetPassword(t.Context(), &out, strings.NewReader(""), AdminSetPasswordCmd{User: "admin"})
	if code != 2 {
		t.Fatalf("exit = %d; want 2 when stdin supplies no password", code)
	}
}

func TestRunAdmin_UnknownSubcommandPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	code := runAdmin(t.Context(), &out, strings.NewReader(""), AdminCmd{})
	if code != 2 {
		t.Fatalf("exit = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "set-password") {
		t.Errorf("usage should name the subcommand; got %q", out.String())
	}
}
