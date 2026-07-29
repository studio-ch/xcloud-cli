// Package config resolves the CLI's runtime settings from, in order of
// precedence and evaluated PER FIELD: an explicit flag, an environment
// variable, the selected profile, then a built-in default.
//
// Per-field resolution is load-bearing, not an implementation detail.
// The standard CI shape is a committed profile that carries api_url plus
// a token injected from the runner's secret store:
//
//	CLOUDCONSOLE_API_TOKEN=$SECRET cloudconsole --profile stage instance list
//
// If precedence were resolved per *source* — "env wins, so ignore the
// profile entirely" — that invocation would silently fall back to the
// production URL. So each field picks its own winner.
//
// Credentials live in a 0600 file rather than the OS keychain. The
// primary environment for this tool is headless (containers, CI runners,
// SSH sessions) where there is no D-Bus and no gnome-keyring, so a
// keyring library falls back to a file anyway — we would ship two code
// paths and exercise one. On macOS the keychain ACL is bound to the code
// signature, so every release would invalidate it and prompt on each
// invocation. The token itself is a ~/.aws/credentials-class secret:
// revocable, scope-limited, tenant-bound and expiring by default. Users
// who want a secret manager get `token_command` instead of a dependency.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultAPIURL is the production Cloud Console API origin. The client
// appends /v1 itself, so this matches what docs/mcp.md already tells
// customers to paste.
const DefaultAPIURL = "https://api.cloud.flow.swiss"

// DefaultOutput is the rendering format when nothing else says otherwise.
const DefaultOutput = "table"

// currentVersion is the config schema version. It is written on save so
// a future breaking change can migrate rather than guess.
const currentVersion = 1

// File is the on-disk YAML document.
type File struct {
	Version        int                 `yaml:"version"`
	CurrentProfile string              `yaml:"current_profile,omitempty"`
	Profiles       map[string]*Profile `yaml:"profiles,omitempty"`
}

// Profile is one named endpoint + credential pairing.
//
// An API key is bound to exactly one tenant server-side (the x-tenant-id
// header is ignored for api_key actors), so there is no tenant switch:
// multiple organisations means multiple profiles. TenantID and KeyPrefix
// are caches written at login so `auth status --offline` and shell
// prompts work without a network round-trip; they are never treated as
// authoritative.
type Profile struct {
	APIURL string `yaml:"api_url,omitempty"`
	Token  string `yaml:"token,omitempty"`
	// TokenCommand is run through the shell and its trimmed stdout used
	// as the token. Lets 1Password/pass/Vault users keep the secret out
	// of the config file without us taking a dependency on any of them.
	TokenCommand string `yaml:"token_command,omitempty"`
	Output       string `yaml:"output,omitempty"`

	TenantID   string `yaml:"tenant_id,omitempty"`
	TenantHint string `yaml:"tenant_hint,omitempty"`
	KeyPrefix  string `yaml:"key_prefix,omitempty"`
}

// Overrides carries the values supplied by global flags. An empty string
// means "not set on the command line", which is why these are strings
// rather than typed values — the flag layer must not invent a default
// that would outrank the profile.
type Overrides struct {
	Profile string
	APIURL  string
	Token   string
	Output  string
}

// Source records where a resolved value came from, so `auth status
// --explain` can show the user why they are talking to the endpoint they
// are talking to. Diagnosing "wrong environment" is otherwise guesswork.
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceProfile Source = "profile"
	SourceDefault Source = "default"
	SourceCommand Source = "token_command"
)

// Resolved is the effective configuration for one command invocation.
type Resolved struct {
	ProfileName string
	APIURL      string
	Token       string
	Output      string

	APIURLFrom  Source
	TokenFrom   Source
	OutputFrom  Source
	ProfileFrom Source

	// TenantID/TenantHint/KeyPrefix are the cached hints from the
	// profile, if any. Commands must re-verify against the API before
	// presenting them as fact.
	TenantID   string
	TenantHint string
	KeyPrefix  string

	Path string // config file this came from ("" if none existed)
}

// ErrNoToken means no credential could be resolved from any source.
var ErrNoToken = errors.New("no API token configured")

// Dir returns the configuration directory, honouring XDG on every
// platform including macOS.
//
// macOS convention would be ~/Library/Application Support, but that is
// guidance for bundled GUI apps. This is a CI-first, SSH-first,
// container-first tool, and every peer CLI a customer already has (gh,
// aws, kubectl, doctl, flyctl) uses a dotdir or XDG on macOS. One path
// across Darwin and Linux is one line in the docs, one line in a
// Dockerfile, one volume mount.
func Dir() (string, error) {
	if v := os.Getenv("CLOUDCONSOLE_CONFIG_HOME"); v != "" {
		return v, nil
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "cloudconsole"), nil
		}
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "cloudconsole"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "cloudconsole"), nil
}

// Path returns the config file path. CLOUDCONSOLE_CONFIG overrides it wholesale
// so a CI job can point at a throwaway file without touching $HOME.
func Path() (string, error) {
	if v := os.Getenv("CLOUDCONSOLE_CONFIG"); v != "" {
		return v, nil
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads the config file. A missing file is not an error — it yields
// an empty document, so a token supplied purely through the environment
// works on a machine that has never run `auth login`.
func Load() (*File, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: currentVersion, Profiles: map[string]*Profile{}}, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}
	if err := checkPerms(path); err != nil {
		return nil, path, err
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	return &f, path, nil
}

// checkPerms refuses a world- or group-readable credential file, in the
// spirit of ssh(1) rejecting a loose private key. Silently reading a
// 0644 file containing a bearer token would be the wrong default.
// CLOUDCONSOLE_INSECURE_CONFIG=1 exists for the odd container image whose
// build step cannot preserve modes.
func checkPerms(path string) error {
	if os.Getenv("CLOUDCONSOLE_INSECURE_CONFIG") == "1" || runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf(
			"config file %s has permissions %#o, which are too open — "+
				"it contains an API token that other users can read.\n"+
				"Fix it with:  chmod 600 %s\n"+
				"(or set CLOUDCONSOLE_INSECURE_CONFIG=1 to bypass this check)",
			path, mode, path)
	}
	return nil
}

// Save writes the config atomically with 0600 permissions.
//
// Atomic because a crash mid-write on the file that holds the only copy
// of a credential would be an unpleasant way to learn about truncation:
// we write a sibling temp file and rename, which is atomic within a
// filesystem.
func Save(f *File) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, fmt.Errorf("create config directory: %w", err)
	}
	f.Version = currentVersion
	body, err := yaml.Marshal(f)
	if err != nil {
		return path, fmt.Errorf("encode config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return path, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return path, fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return path, fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return path, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return path, fmt.Errorf("replace %s: %w", path, err)
	}
	return path, nil
}

// Resolve applies the precedence chain, per field.
func Resolve(f *File, o Overrides) (*Resolved, error) {
	r := &Resolved{}

	// Which profile, before any of its fields are read.
	switch {
	case o.Profile != "":
		r.ProfileName, r.ProfileFrom = o.Profile, SourceFlag
	case os.Getenv("CLOUDCONSOLE_PROFILE") != "":
		r.ProfileName, r.ProfileFrom = os.Getenv("CLOUDCONSOLE_PROFILE"), SourceEnv
	case f.CurrentProfile != "":
		r.ProfileName, r.ProfileFrom = f.CurrentProfile, SourceProfile
	default:
		r.ProfileName, r.ProfileFrom = "default", SourceDefault
	}

	p := f.Profiles[r.ProfileName]
	// An explicitly requested profile that does not exist is an error,
	// not a silent fallback: acting on the wrong tenant because of a
	// typo is exactly the failure this check prevents. An *implicit*
	// name (the built-in "default") is allowed to be absent, so a
	// pure-environment invocation still works.
	if p == nil {
		if r.ProfileFrom == SourceFlag || r.ProfileFrom == SourceEnv {
			return nil, fmt.Errorf("profile %q not found in the config file", r.ProfileName)
		}
		p = &Profile{}
	}

	r.APIURL, r.APIURLFrom = pick(o.APIURL, os.Getenv("CLOUDCONSOLE_API_URL"), p.APIURL, DefaultAPIURL)
	r.APIURL = normalizeAPIURL(r.APIURL)

	r.Output, r.OutputFrom = pick(o.Output, os.Getenv("CLOUDCONSOLE_OUTPUT"), p.Output, DefaultOutput)

	// The token has a fourth possible source, token_command, which sits
	// between the profile's literal token and the (nonexistent) default.
	switch {
	case o.Token != "":
		r.Token, r.TokenFrom = o.Token, SourceFlag
	case os.Getenv("CLOUDCONSOLE_API_TOKEN") != "":
		r.Token, r.TokenFrom = os.Getenv("CLOUDCONSOLE_API_TOKEN"), SourceEnv
	case p.Token != "":
		r.Token, r.TokenFrom = p.Token, SourceProfile
	case p.TokenCommand != "":
		tok, err := runTokenCommand(p.TokenCommand)
		if err != nil {
			return nil, err
		}
		r.Token, r.TokenFrom = tok, SourceCommand
	}

	r.TenantID, r.TenantHint, r.KeyPrefix = p.TenantID, p.TenantHint, p.KeyPrefix
	return r, nil
}

// pick returns the first non-empty value and where it came from.
func pick(flag, env, profile, def string) (string, Source) {
	switch {
	case flag != "":
		return flag, SourceFlag
	case env != "":
		return env, SourceEnv
	case profile != "":
		return profile, SourceProfile
	default:
		return def, SourceDefault
	}
}

// normalizeAPIURL accepts an origin with or without a trailing /v1 and
// returns the bare origin. The client appends /v1 itself; accepting both
// spellings means a customer who copies the base URL out of the MCP docs
// (which include /v1) does not get a /v1/v1 404.
func normalizeAPIURL(u string) string {
	u = strings.TrimRight(u, "/")
	return strings.TrimSuffix(u, "/v1")
}

// runTokenCommand executes the profile's token_command and returns its
// trimmed stdout. Stderr is passed through so a locked 1Password vault
// can tell the user what it needs; stdout never is, since it is the
// secret.
func runTokenCommand(command string) (string, error) {
	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd := exec.Command(shell, flag, command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token_command failed: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", errors.New("token_command produced no output")
	}
	return tok, nil
}
