package main

import (
	"strings"
	"testing"
)

func TestParseUploadResponse(t *testing.T) {
	const site = "https://filestore.me"

	cases := []struct {
		name     string
		body     string
		wantCode string
		wantErr  string
	}{
		{"standard array", `[{"file_code":"ba8q777pa1bj","file_status":"OK","file_name":"a.zip"}]`, "ba8q777pa1bj", ""},
		{"single object", `{"file_code":"abc123def456","file_status":"OK"}`, "abc123def456", ""},
		{"invalid session", `[{"file_code":"","file_status":"Invalid session"}]`, "", "Invalid session"},
		{"account not enabled", `[{"file_status":"uploads are not enabled for your account type"}]`, "", "not enabled"},
		{"non-JSON containing a code", `<html>file_code: q7w8e9r0t1x2 done</html>`, "q7w8e9r0t1x2", ""},
		{"empty", ``, "", "returned nothing"},
		{"unparseable", `<html>errore generico</html>`, "", "cannot parse"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, code, err := parseUploadResponse([]byte(tc.body), site)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got link %q", tc.wantErr, link)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != tc.wantCode {
				t.Fatalf("code %q, expected %q", code, tc.wantCode)
			}
			if want := site + "/" + tc.wantCode; link != want {
				t.Fatalf("link %q, expected %q", link, want)
			}
		})
	}
}

// Application-level errors must not be retried: re-uploading a rejected file
// wastes bandwidth and time.
func TestRetryable(t *testing.T) {
	for _, s := range []string{"uploads are not enabled for your account type", "Invalid key", "Invalid session", "file too large"} {
		if retryable(errString(s)) {
			t.Errorf("%q should not be retried", s)
		}
	}
	for _, s := range []string{"rete durante l'upload: connection reset", "EOF inatteso", "timeout"} {
		if !retryable(errString(s)) {
			t.Errorf("%q should be retried", s)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		1048576: "1.0 MB", 24186265: "23.1 MB", 10737418240: "10.0 GB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, expected %q", in, got, want)
		}
	}
}

func TestEscapeQuotes(t *testing.T) {
	if got := escapeQuotes(`nome"con"virgolette.mkv`); got != `nome\"con\"virgolette.mkv` {
		t.Errorf("wrong escaping: %q", got)
	}
	if got := escapeQuotes("con\r\nnewline.txt"); got != "connewline.txt" {
		t.Errorf("newlines must be stripped, got %q", got)
	}
}

// In numbered series the prefix is identical, so trimming the tail would hide
// the only part that tells the files apart.
func TestTruncateMiddle(t *testing.T) {
	const name = "CSA_B8hxFFh_1000h_part_0042.mkv"

	if got := truncateMiddle(name, 40); got != name {
		t.Errorf("a short name must be left alone: %q", got)
	}
	got := truncateMiddle(name, 22)
	if len([]rune(got)) != 22 {
		t.Errorf("length %d, expected 22: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "0042.mkv") {
		t.Errorf("the distinguishing tail must stay visible, got %q", got)
	}
	if !strings.HasPrefix(got, "CSA_") {
		t.Errorf("the beginning must stay recognisable too, got %q", got)
	}
}

// A line wider than the terminal wraps onto a second physical row, which breaks
// the cursor arithmetic and smears the display down the screen.
func TestFitVisibleIgnoresAnsi(t *testing.T) {
	plain := "abcdefghij"
	if got := fitVisible(plain, 4); got != "abcd" {
		t.Errorf("plain truncation = %q, expected \"abcd\"", got)
	}
	coloured := "\033[31mabcdefghij\033[0m"
	got := fitVisible(coloured, 4)
	if !strings.Contains(got, "\033[31m") {
		t.Errorf("the colour code must be preserved: %q", got)
	}
	if visible := stripAnsiForTest(got); visible != "abcd" {
		t.Errorf("visible part = %q, expected \"abcd\"", visible)
	}
	if got := fitVisible(plain, 50); got != plain {
		t.Errorf("a short line must be left alone: %q", got)
	}
}

func stripAnsiForTest(s string) string {
	var out []rune
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == 0x1b {
			for i < len(runes) {
				r := runes[i]
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
					break
				}
				i++
			}
			continue
		}
		out = append(out, runes[i])
	}
	return string(out)
}

func TestParseServerLimit(t *testing.T) {
	cases := map[string]int64{
		"ERROR: Max filesize limit exceeded! Filesize limit: 10 Mb": 10 << 20,
		"Filesize limit: 2 Gb":  2 << 30,
		"filesize limit:512 Kb": 512 << 10,
		"some unrelated error":  0,
	}
	for msg, want := range cases {
		if got := parseServerLimit(msg); got != want {
			t.Errorf("parseServerLimit(%q) = %d, expected %d", msg, got, want)
		}
	}
}

// The server's size rejection is final: retrying re-sends the whole file for
// a refusal that will never change.
func TestSizeLimitIsNotRetryable(t *testing.T) {
	if retryable(errString("ERROR: Max filesize limit exceeded! Filesize limit: 10 Mb")) {
		t.Error("a filesize-limit rejection must never be retried")
	}
}
