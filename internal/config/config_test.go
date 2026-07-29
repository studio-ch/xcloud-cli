package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolate points the config machinery at a throwaway file and clears
// every environment variable that participates in resolution, so a
// developer's real ~/.config/xcloud/config.yaml can never influence a
// test run (nor be written to by one).
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("XCLOUD_CONFIG", path)
	for _, k := range []string{
		"XCLOUD_PROFILE", "XCLOUD_API_URL", "XCLOUD_API_TOKEN",
		"XCLOUD_OUTPUT", "XCLOUD_CONFIG_HOME", "XCLOUD_INSECURE_CONFIG",
	} {
		t.Setenv(k, "")
	}
	return path
}

func TestResolvePrecedencePerField(t *testing.T) {
	f := &File{
		CurrentProfile: "prod",
		Profiles: map[string]*Profile{
			"prod":  {APIURL: "https://prod.example", Token: "sk_live_prodtoken00", Output: "table"},
			"stage": {APIURL: "https://stage.example", Token: "sk_live_stagetoken0", Output: "json"},
		},
	}

	tests := []struct {
		name       string
		env        map[string]string
		overrides  Overrides
		wantURL    string
		wantToken  string
		wantOutput string
		wantSrcURL Source
		wantSrcTok Source
	}{
		{
			name:       "profile only",
			wantURL:    "https://prod.example",
			wantToken:  "sk_live_prodtoken00",
			wantOutput: "table",
			wantSrcURL: SourceProfile,
			wantSrcTok: SourceProfile,
		},
		{
			// The load-bearing CI case: a token from the runner's secret
			// store must combine with the *profile's* URL, not drag the
			// default production URL in with it.
			name:       "env token keeps profile url",
			env:        map[string]string{"XCLOUD_API_TOKEN": "sk_live_envtoken000"},
			overrides:  Overrides{Profile: "stage"},
			wantURL:    "https://stage.example",
			wantToken:  "sk_live_envtoken000",
			wantOutput: "json",
			wantSrcURL: SourceProfile,
			wantSrcTok: SourceEnv,
		},
		{
			name:       "flag beats env beats profile",
			env:        map[string]string{"XCLOUD_API_URL": "https://env.example"},
			overrides:  Overrides{APIURL: "https://flag.example"},
			wantURL:    "https://flag.example",
			wantToken:  "sk_live_prodtoken00",
			wantOutput: "table",
			wantSrcURL: SourceFlag,
			wantSrcTok: SourceProfile,
		},
		{
			name:       "env beats profile",
			env:        map[string]string{"XCLOUD_API_URL": "https://env.example"},
			wantURL:    "https://env.example",
			wantToken:  "sk_live_prodtoken00",
			wantOutput: "table",
			wantSrcURL: SourceEnv,
			wantSrcTok: SourceProfile,
		},
		{
			name:       "env profile selection",
			env:        map[string]string{"XCLOUD_PROFILE": "stage"},
			wantURL:    "https://stage.example",
			wantToken:  "sk_live_stagetoken0",
			wantOutput: "json",
			wantSrcURL: SourceProfile,
			wantSrcTok: SourceProfile,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := Resolve(f, tt.overrides)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.APIURL != tt.wantURL {
				t.Errorf("APIURL = %q, want %q", got.APIURL, tt.wantURL)
			}
			if got.Token != tt.wantToken {
				t.Errorf("Token = %q, want %q", got.Token, tt.wantToken)
			}
			if got.Output != tt.wantOutput {
				t.Errorf("Output = %q, want %q", got.Output, tt.wantOutput)
			}
			if got.APIURLFrom != tt.wantSrcURL {
				t.Errorf("APIURLFrom = %q, want %q", got.APIURLFrom, tt.wantSrcURL)
			}
			if got.TokenFrom != tt.wantSrcTok {
				t.Errorf("TokenFrom = %q, want %q", got.TokenFrom, tt.wantSrcTok)
			}
		})
	}
}

func TestResolveDefaults(t *testing.T) {
	isolate(t)
	got, err := Resolve(&File{Profiles: map[string]*Profile{}}, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q, want %q", got.APIURL, DefaultAPIURL)
	}
	if got.Output != DefaultOutput {
		t.Errorf("Output = %q, want %q", got.Output, DefaultOutput)
	}
	if got.Token != "" {
		t.Errorf("Token = %q, want empty", got.Token)
	}
}

// An explicitly named profile that does not exist must fail loudly.
// Falling back silently would run the command against whatever tenant
// happened to be configured, which is the worst possible outcome of a
// typo.
func TestResolveUnknownExplicitProfileErrors(t *testing.T) {
	isolate(t)
	f := &File{Profiles: map[string]*Profile{"prod": {}}}

	if _, err := Resolve(f, Overrides{Profile: "typo"}); err == nil {
		t.Error("expected an error for an unknown --profile, got nil")
	}

	t.Setenv("XCLOUD_PROFILE", "typo")
	if _, err := Resolve(f, Overrides{}); err == nil {
		t.Error("expected an error for an unknown XCLOUD_PROFILE, got nil")
	}
}

func TestNormalizeAPIURL(t *testing.T) {
	tests := map[string]string{
		"https://api.cloud.flow.swiss":     "https://api.cloud.flow.swiss",
		"https://api.cloud.flow.swiss/":    "https://api.cloud.flow.swiss",
		"https://api.cloud.flow.swiss/v1":  "https://api.cloud.flow.swiss",
		"https://api.cloud.flow.swiss/v1/": "https://api.cloud.flow.swiss",
		"http://localhost:3001":            "http://localhost:3001",
	}
	for in, want := range tests {
		if got := normalizeAPIURL(in); got != want {
			t.Errorf("normalizeAPIURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveCreates0600AndRoundTrips(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	path := isolate(t)

	in := &File{
		CurrentProfile: "prod",
		Profiles: map[string]*Profile{
			"prod": {APIURL: "https://prod.example", Token: "sk_live_secret00000", KeyPrefix: "sk_live_secret000"},
		},
	}
	if _, err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}

	out, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.Version != currentVersion {
		t.Errorf("Version = %d, want %d", out.Version, currentVersion)
	}
	if out.Profiles["prod"].Token != "sk_live_secret00000" {
		t.Errorf("token did not round-trip: %q", out.Profiles["prod"].Token)
	}
}

// Reading a world-readable credential file must fail rather than
// silently proceed — same posture as ssh(1) with a loose private key.
func TestLoadRefusesOverPermissiveFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	path := isolate(t)
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	if err == nil {
		t.Fatal("expected an error for a 0644 config file, got nil")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error should tell the user how to fix it, got: %v", err)
	}

	t.Setenv("XCLOUD_INSECURE_CONFIG", "1")
	if _, _, err := Load(); err != nil {
		t.Errorf("XCLOUD_INSECURE_CONFIG=1 should bypass the check, got: %v", err)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	f, _, err := Load()
	if err != nil {
		t.Fatalf("Load on a missing file should succeed, got: %v", err)
	}
	if f.Profiles == nil {
		t.Error("Profiles should be initialised, not nil")
	}
}

// Save must not leave a partially written credential file behind. We
// approximate a crash by checking that no temp file survives a
// successful save and that the target is complete.
func TestSaveLeavesNoTempFiles(t *testing.T) {
	path := isolate(t)
	if _, err := Save(&File{Profiles: map[string]*Profile{"a": {Token: "sk_live_aaaaaaaaaaaa"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("temp file %q survived a successful save", e.Name())
		}
	}
}

func TestTokenCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	isolate(t)
	f := &File{
		CurrentProfile: "vault",
		Profiles:       map[string]*Profile{"vault": {TokenCommand: "echo sk_live_fromcommand0"}},
	}
	got, err := Resolve(f, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Token != "sk_live_fromcommand0" {
		t.Errorf("Token = %q, want sk_live_fromcommand0", got.Token)
	}
	if got.TokenFrom != SourceCommand {
		t.Errorf("TokenFrom = %q, want %q", got.TokenFrom, SourceCommand)
	}
}

func TestTokenCommandFailurePropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	isolate(t)
	f := &File{
		CurrentProfile: "vault",
		Profiles:       map[string]*Profile{"vault": {TokenCommand: "exit 1"}},
	}
	if _, err := Resolve(f, Overrides{}); err == nil {
		t.Error("expected an error when token_command fails, got nil")
	}
}

// A literal token in the profile outranks token_command: it is the more
// specific statement of intent, and it avoids shelling out on every
// single command when both happen to be set.
func TestLiteralTokenBeatsTokenCommand(t *testing.T) {
	isolate(t)
	f := &File{
		CurrentProfile: "p",
		Profiles: map[string]*Profile{
			"p": {Token: "sk_live_literal00000", TokenCommand: "echo sk_live_fromcommand0"},
		},
	}
	got, err := Resolve(f, Overrides{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Token != "sk_live_literal00000" {
		t.Errorf("Token = %q, want the literal token", got.Token)
	}
}

func TestDirHonoursXDG(t *testing.T) {
	t.Setenv("XCLOUD_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if runtime.GOOS == "windows" {
		t.Skip("windows uses APPDATA")
	}
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/tmp/xdg", "xcloud"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// ExtractKeyPrefix must match apps/api/src/lib/api-keys.ts exactly. The
// `_`/`-` cases are the ones that break a naive strings.Split(s, "_").
func TestExtractKeyPrefixMatchesServer(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"sk_live_abcdefghijkl" + "MNOPQRSTUVWXYZ0123456789abcdefghij", "sk_live_abcdefghijkl"},
		{"sk_test_abcdefghijkl" + "rest", "sk_test_abcdefghijkl"},
		// base64url alphabet inside the random part — the reason we
		// cannot split on "_".
		{"sk_live_a_b-c_d-e_fg" + "hijklmnop", "sk_live_a_b-c_d-e_fg"},
		{"sk_live____________" + "xyz", "sk_live____________x"},
		{"sk_live_-----------" + "-abc", "sk_live_------------"},
		// Too short to carry a 12-char lookup prefix.
		{"sk_live_short", ""},
		{"sk_live_", ""},
		// Wrong or missing scheme.
		{"pk_live_abcdefghijkl", ""},
		{"abcdefghijkl", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ExtractKeyPrefix(tt.in); got != tt.want {
			t.Errorf("ExtractKeyPrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
		// Every accepted key yields a prefix of exactly len("sk_live_")+12.
		if tt.want != "" && len(tt.want) != len("sk_live_")+12 {
			t.Errorf("test case is wrong: %q is not 8+12 chars", tt.want)
		}
	}
}
