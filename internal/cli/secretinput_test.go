package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/exitcode"
)

func TestReadUserSecret(t *testing.T) {
	const secret = "Synth0Secret0Value0123456789abcd"
	t.Run("piped stdin is read to EOF", func(t *testing.T) {
		a, _, stderr := testApp(t)
		a.stdinTerminal = func() bool { return false }
		a.Stdin = strings.NewReader(secret + "\n")
		got, err := a.readUserSecret(context.Background(), "Auth0 client secret", "stdin")
		if err != nil || string(got) != secret || stderr.Len() != 0 {
			t.Errorf("got %q, %v; stderr %q", got, err, stderr)
		}
	})
	t.Run("a terminal is prompted and read hidden", func(t *testing.T) {
		a, _, stderr := testApp(t)
		a.stdinTerminal = func() bool { return true }
		a.readHidden = func(context.Context, io.Reader) ([]byte, error) { return []byte(secret), nil }
		got, err := a.readUserSecret(context.Background(), "Auth0 client secret", "stdin")
		if err != nil || string(got) != secret {
			t.Errorf("got %q, %v", got, err)
		}
		if want := "Paste the Auth0 client secret and press Enter (input is hidden): \n"; stderr.String() != want {
			t.Errorf("stderr %q, want %q", stderr, want)
		}
	})
	t.Run("an empty line is refused", func(t *testing.T) {
		a, _, _ := testApp(t)
		a.stdinTerminal = func() bool { return true }
		a.readHidden = func(context.Context, io.Reader) ([]byte, error) { return nil, nil }
		if _, err := a.readUserSecret(context.Background(), "x", "stdin"); exitcode.FromError(err) != exitcode.CodeUsage {
			t.Errorf("err = %v", err)
		}
	})
	for _, flag := range []string{"--json", "--non-interactive"} {
		t.Run("a terminal with "+flag, func(t *testing.T) {
			a, _, stderr := testApp(t)
			a.Global.JSON = flag == "--json"
			a.Global.NonInteractive = flag == "--non-interactive"
			a.stdinTerminal = func() bool { return true }
			a.readHidden = func(context.Context, io.Reader) ([]byte, error) { return nil, errors.New("read") }
			_, err := a.readUserSecret(context.Background(), "x", "--db-root-password-stdin")
			if exitcode.FromError(err) != exitcode.CodeUsage || !strings.Contains(err.Error(), flag+" forbids prompting; pipe the secret in") {
				t.Errorf("err = %v", err)
			}
			if stderr.Len() != 0 {
				t.Errorf("prompted: %q", stderr)
			}
		})
	}
}

func TestReadRawLine(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		code     int
	}{
		{in: "secret\r", want: "secret"},
		{in: "secret\n", want: "secret"},
		{in: "secrex\x7ft\r", want: "secret"},
		{in: "s\u00e9\x7f\x7fsecret\r", want: "secret"},
		{in: "wrong\x15secret\r", want: "secret"},
		{in: "sec\x03", code: exitcode.CodeInterrupted},
	} {
		got, err := readRawLine(strings.NewReader(tc.in))
		if string(got) != tc.want || exitcode.FromError(err) != tc.code {
			t.Errorf("%q: got %q, %v", tc.in, got, err)
		}
	}
	if _, err := readRawLine(strings.NewReader("\x04")); err != io.EOF {
		t.Errorf("Ctrl-D: %v", err)
	}
}
