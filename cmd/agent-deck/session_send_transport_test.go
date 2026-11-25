package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestEffectiveSendTransport(t *testing.T) {
	previous := loadUserConfigForSend
	t.Cleanup(func() { loadUserConfigForSend = previous })
	for _, tc := range []struct {
		name, flagValue, configValue, want string
		configErr                          error
		wantErr                            string
	}{
		{name: "config auto", configValue: "auto", want: "auto"},
		{name: "config unset", want: "tmux"},
		{name: "pin tmux", flagValue: "tmux", configValue: "auto", want: "tmux"},
		{name: "opt in", flagValue: "auto", want: "auto"},
		{name: "skip broken config", flagValue: "auto", configErr: errors.New("malformed TOML"), want: "auto"},
		{name: "invalid", flagValue: "socket", wantErr: `--transport must be "tmux" or "auto"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configLoads := 0
			loadUserConfigForSend = func() (*session.UserConfig, error) {
				configLoads++
				return &session.UserConfig{SendTransport: tc.configValue}, tc.configErr
			}
			value, warn, err := effectiveSendTransport(tc.flagValue)
			errText := ""
			if err != nil {
				errText = err.Error()
			}
			if value != tc.want || warn != "" || errText != tc.wantErr {
				t.Fatalf("effectiveSendTransport(%q) = (%q, %q, %v), want (%q, no warning, %q)", tc.flagValue, value, warn, err, tc.want, tc.wantErr)
			}
			if (tc.flagValue == "" && configLoads != 1) || (tc.flagValue != "" && configLoads != 0) {
				t.Fatalf("unexpected config loads: %d", configLoads)
			}
		})
	}
}

func TestChildSendArgsTransportOverride(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts childSendOptions
	}{
		{name: "plain"},
		{name: "guarded", opts: childSendOptions{requireInputPrompt: true}},
		{name: "released", opts: childSendOptions{requireInputPrompt: true, noWait: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := append(childSendArgs("test-profile", "target", "message", tc.opts), "--transport", "tmux")
			opts := tc.opts
			opts.transportOverride = "tmux"
			got := childSendArgs("test-profile", "target", "message", opts)
			if !slices.Equal(got, want) {
				t.Fatalf("childSendArgs = %q, want %q", got, want)
			}
		})
	}
}

func TestQueuedSendTransportOverride(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             string
		requireInputPrompt bool
		wantOptions        childSendOptions
	}{
		{name: "plain", status: "idle", wantOptions: childSendOptions{transportOverride: "tmux"}},
		{
			name: "guarded", status: "idle", requireInputPrompt: true,
			wantOptions: childSendOptions{requireInputPrompt: true, transportOverride: "tmux"},
		},
		{
			name: "released", status: queueReleaseStatus,
			wantOptions: childSendOptions{requireInputPrompt: true, noWait: true, transportOverride: "tmux"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, variant := range []struct {
				name, override, wantLauncher string
			}{
				{name: "override", override: "tmux", wantLauncher: "with options"},
				{name: "legacy", wantLauncher: tc.name},
			} {
				t.Run(variant.name, func(t *testing.T) {
					previous, previousGuarded, previousRelease, previousWithOptions := sendChild, sendChildGuarded, sendChildRelease, sendChildWithOptions
					t.Cleanup(func() {
						sendChild, sendChildGuarded, sendChildRelease, sendChildWithOptions = previous, previousGuarded, previousRelease, previousWithOptions
					})
					launcher := ""
					stub := func(name string) func(string, string, string, string) (int, func() int, error) {
						return func(_, _, _, _ string) (int, func() int, error) {
							if name != variant.wantLauncher {
								t.Fatalf("used %s launcher, want %s", name, variant.wantLauncher)
							}
							launcher = name
							return 1, func() int { return 0 }, nil
						}
					}
					sendChild = stub("plain")
					sendChildGuarded = stub("guarded")
					sendChildRelease = stub("released")
					withOptions := stub("with options")
					sendChildWithOptions = func(profile, id, message, resultPath string, opts childSendOptions) (int, func() int, error) {
						if variant.override == "" {
							t.Fatal("legacy record called sendChildWithOptions")
						}
						if opts != tc.wantOptions {
							t.Fatalf("sendChildWithOptions got %+v, want %+v", opts, tc.wantOptions)
						}
						return withOptions(profile, id, message, resultPath)
					}
					rec := &sendqueue.Record{
						SendID: sendqueue.NewID(time.Now()), SessionID: "target",
						RequireInputPrompt: tc.requireInputPrompt, TransportOverride: variant.override,
					}
					set := func(update func(*sendqueue.Record)) error {
						update(rec)
						return nil
					}
					if !typeQueued("", t.TempDir(), rec, tc.status, "", 0, set) || launcher != variant.wantLauncher {
						t.Fatalf("typeQueued used launcher %q, want %q", launcher, variant.wantLauncher)
					}
				})
			}
		})
	}
}

func TestQueueSendPersistsTransportOverride(t *testing.T) {
	const helper = "AGENTDECK_QUEUE_SEND_TRANSPORT_TEST"
	const profile = "ch_support_test"
	if os.Getenv(helper) == "1" {
		storage, err := session.NewStorageWithProfile(profile)
		if err != nil {
			t.Fatal(err)
		}
		// A stopped target makes queueSend exit after saving, without launching a worker.
		queueSend(profile, storage, &session.Instance{ID: "target"}, "message", nil, false, "", false, "tmux", NewCLIOutput(false, false))
		t.Fatal("queueSend returned for a stopped target")
	}

	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestQueueSendPersistsTransportOverride$")
	cmd.Env = agentDeckTestEnv(home, []string{helper + "=1", "AGENT_DECK_TASK6_HELPER_PROCESS=1"})
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(output), "target not running") {
		t.Fatalf("queueSend exit = %v, output = %s", err, output)
	}
	dir := sendqueue.Dir(filepath.Join(home, ".local", "share", "agent-deck", "profiles", profile))
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("queue record files = %v, err = %v, want one file", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		TransportOverride string `json:"transport_override"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.TransportOverride != "tmux" {
		t.Fatalf("saved transport_override = %q, want tmux", saved.TransportOverride)
	}
}

func TestSessionSendRejectsEmptyTransport(t *testing.T) {
	const helper = "AGENTDECK_EMPTY_SEND_TRANSPORT_TEST"
	if os.Getenv(helper) == "1" {
		handleSessionSend("default", []string{"target", "--message-file", "missing-message.txt", "--transport="})
		return
	}

	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSessionSendRejectsEmptyTransport$")
	cmd.Dir = home
	cmd.Env = agentDeckTestEnv(home, []string{helper + "=1", "AGENT_DECK_TASK6_HELPER_PROCESS=1"})
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	const want = "Error: --transport must be \"tmux\" or \"auto\"\n"
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || string(output) != want {
		t.Fatalf("session send exit = %v, output = %q, want exit 1 and %q", err, output, want)
	}
}

func TestRemoteSendTransportTakesAValue(t *testing.T) {
	args := []string{"session", "send", "target", "--transport", "--message-file", "/missing"}
	forwarded, _, closeInput, err := remoteMessageInput(args)
	defer closeInput()
	if err != nil || strings.Join(forwarded, " ") != strings.Join(args, " ") {
		t.Fatalf("remoteMessageInput = %v, %v, want original arguments", forwarded, err)
	}
}

// alwaysSocketOK is a resolve stub that always succeeds, so any row using it
// reaches chooseSendTransport's final "otherwise" branch when nothing earlier
// routes to tmux. resolveCalls counts invocations so tests can assert
// resolve runs exactly once per chooseSendTransport call (NIT: one resolve
// per send, not two).
func alwaysSocketOK(resolveCalls *int) func() (send.ClaudeSocketTarget, error) {
	return func() (send.ClaudeSocketTarget, error) {
		if resolveCalls != nil {
			*resolveCalls++
		}
		return send.ClaudeSocketTarget{SocketPath: "/tmp/whatever.sock", Pid: 1, SessionID: "sid"}, nil
	}
}

// alwaysSocketUnavailable is a resolve stub simulating a specific pre-write
// refusal, e.g. a dead pid.
func alwaysSocketUnavailable(reason send.UnavailableReason) func() (send.ClaudeSocketTarget, error) {
	return func() (send.ClaudeSocketTarget, error) {
		return send.ClaudeSocketTarget{}, &send.Unavailable{Reason: reason}
	}
}

func TestChooseSendTransport(t *testing.T) {
	cases := []struct {
		name          string
		in            transportInputs
		wantTransport sendTransport
		wantReason    send.UnavailableReason
	}{
		{
			name: "SSH-backed instance always takes tmux, even with an otherwise-good record",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "hello",
				claudeSessionID: "sid", isSSH: true, resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonRemoteSession,
		},
		{
			name: "explicit tmux pin wins over an otherwise-good record",
			in: transportInputs{
				tool: "claude", configValue: "tmux", message: "hello",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonConfigPinnedTmux,
		},
		{
			name: "codex is not Claude-compatible",
			in: transportInputs{
				tool: "codex", configValue: "auto", message: "hello",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonNotClaudeCompatible,
		},
		{
			name: "opencode is not Claude-compatible",
			in: transportInputs{
				tool: "opencode", configValue: "auto", message: "hello",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonNotClaudeCompatible,
		},
		{
			name: "bare slash command routes to tmux",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "/compact",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonSlashCommand,
		},
		{
			name: "slash command with leading whitespace still routes to tmux",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "  /help",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonSlashCommand,
		},
		{
			name: "a path that merely starts with / is not a slash command message here (message body, not composer literal) but still matches the safe prefix rule",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "/Users/tarek/notes.md please read this",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			// §7.5: the predicate is a prefix match, so a message beginning
			// with an absolute path also routes to tmux. Safe direction
			// (status quo), documented quirk.
			wantTransport: transportTmux, wantReason: reasonSlashCommand,
		},
		{
			name: "plain text is not a slash command",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "not/a/slash because it has no leading slash",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportSocket, wantReason: "",
		},
		{
			name: "no known claude session id routes to tmux",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "hello",
				claudeSessionID: "", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonNoClaudeSessionID,
		},
		{
			name: "resolve() unavailable (dead pid) surfaces that exact reason",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "hello",
				claudeSessionID: "sid", resolve: alwaysSocketUnavailable(send.ReasonDeadPid),
			},
			wantTransport: transportTmux, wantReason: send.ReasonDeadPid,
		},
		{
			name: "resolve() unavailable (old protocol) surfaces that exact reason",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "hello",
				claudeSessionID: "sid", resolve: alwaysSocketUnavailable(send.ReasonOldProtocol),
			},
			wantTransport: transportTmux, wantReason: send.ReasonOldProtocol,
		},
		{
			name: "everything clear -> socket, no reason",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "do the thing",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportSocket, wantReason: "",
		},
		{
			// The socket is opt-in: only a literal "auto" selects it, so an
			// empty value takes tmux here too, not just at
			// sendTransportFromConfig (maintainer review of #2100).
			name: "config value empty string takes tmux, not socket",
			in: transportInputs{
				tool: "claude", configValue: "", message: "do the thing",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			// Nobody opted in; that is not the same fact as an explicit pin.
			wantTransport: transportTmux, wantReason: reasonTransportNotOptedIn,
		},
		{
			name: "unrecognized config value takes tmux",
			in: transportInputs{
				tool: "claude", configValue: "AUTO", message: "do the thing",
				claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
			},
			wantTransport: transportTmux, wantReason: reasonTransportNotOptedIn,
		},
		{
			name: "nil resolve seam reports a selector-level reason",
			in: transportInputs{
				tool: "claude", configValue: "auto", message: "do the thing",
				claudeSessionID: "sid", resolve: nil,
			},
			wantTransport: transportTmux, wantReason: reasonNoResolver,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTransport, gotReason, gotTarget := chooseSendTransport(tc.in)
			if gotTransport != tc.wantTransport {
				t.Errorf("transport = %q, want %q", gotTransport, tc.wantTransport)
			}
			if gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
			if tc.wantTransport == transportSocket && gotTarget.SocketPath == "" {
				t.Errorf("expected a resolved target on a socket decision, got zero value")
			}
			if tc.wantTransport == transportTmux && gotTarget != (send.ClaudeSocketTarget{}) {
				t.Errorf("expected a zero-value target on a tmux decision, got %+v", gotTarget)
			}
		})
	}
}

// TestChooseSendTransport_ResolvesExactlyOnce guards the NIT fix: earlier
// code called resolve() once inside chooseSendTransport and again inside
// executeSocketSend, doubling the ~/.claude/sessions scan and the `ps`
// fork per socket send. chooseSendTransport must be the only caller.
func TestChooseSendTransport_ResolvesExactlyOnce(t *testing.T) {
	var calls int
	transport, _, target := chooseSendTransport(transportInputs{
		tool: "claude", configValue: "auto", message: "hello",
		claudeSessionID: "sid", resolve: alwaysSocketOK(&calls),
	})
	if transport != transportSocket {
		t.Fatalf("transport = %q, want socket", transport)
	}
	if calls != 1 {
		t.Errorf("resolve called %d times, want exactly 1", calls)
	}
	if target.SocketPath != "/tmp/whatever.sock" {
		t.Errorf("target = %+v, want the resolved target threaded through", target)
	}
}

// TestChooseSendTransport_ClaudeCompatibleAlias covers a custom tool defined
// with compatible_with = "claude" (session.IsClaudeCompatible's own
// aliasing), proving chooseSendTransport treats it exactly like the "claude"
// built-in rather than only recognizing the literal string.
func TestChooseSendTransport_ClaudeCompatibleAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &session.UserConfig{
		Tools: map[string]session.ToolDef{
			"claude_wrapper": {Command: "claude-wrapper", CompatibleWith: "claude"},
		},
	}
	if err := session.SaveUserConfig(cfg); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	session.ClearUserConfigCache()

	transport, reason, _ := chooseSendTransport(transportInputs{
		tool: "claude_wrapper", configValue: "auto", message: "hello",
		claudeSessionID: "sid", resolve: alwaysSocketOK(nil),
	})
	if transport != transportSocket || reason != "" {
		t.Errorf("claude-compatible alias: transport=%q reason=%q, want socket/\"\"", transport, reason)
	}
}

// TestPerformSend_SSHBackedInstance_TakesTmuxWithEmptyFallbackReason is the
// end-to-end proof behind the "SSH-backed instance always takes tmux" table
// row above: an otherwise-perfectly-resolvable local Claude session record
// (alwaysSocketOK never fails) still routes to tmux for a remote instance,
// and — because reasonRemoteSession is a selector-level reason, filtered by
// runTmuxSend just like an explicit send_transport=tmux pin — the resulting
// sendDeliveryResult.fallbackReason (the --json fallback_reason field) stays
// empty rather than leaking an internal-only reason string.
func TestPerformSend_SSHBackedInstance_TakesTmuxWithEmptyFallbackReason(t *testing.T) {
	mock := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{""}}
	inst := &session.Instance{ID: "i1", Title: "target", Tool: "claude", ClaudeSessionID: "sid", SSHHost: "example.com"}
	if !inst.IsSSH() {
		t.Fatal("test setup: inst should be SSH-backed")
	}

	res, err := performSend(inst, mock, "hello", false, defaultSendTuning(), "auto", false, nil, instResolveOK, nil)
	if err != nil {
		t.Fatalf("performSend: %v", err)
	}
	if res.transport != "tmux" {
		t.Errorf("transport = %q, want %q (SSH-backed instance must never use the socket)", res.transport, "tmux")
	}
	if res.fallbackReason != "" {
		t.Errorf("fallbackReason = %q, want empty (remote_session is a selector-level reason, not a socket-refusal reason)", res.fallbackReason)
	}
	if mock.sendKeysCalls == 0 && mock.sendChunkedCalls == 0 {
		t.Errorf("expected the tmux target to be exercised for an SSH-backed instance")
	}
}

// TestSendTransportFromConfig is the regression test for the CodeRabbit
// finding: a malformed config.toml makes LoadUserConfig return a default
// config (GetSendTransport() would say "auto") PLUS a non-nil error, and the
// original code silently ignored that error, defaulting to "auto" even for
// a user who had pinned send_transport = "tmux" — the pin looked like it
// silently stopped applying whenever the file happened to have an unrelated
// typo. sendTransportFromConfig must treat a load error as "tmux"
// (conservative) and report a one-line warning.
func TestSendTransportFromConfig(t *testing.T) {
	t.Run("load error -> tmux, with a warning", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadErr := errors.New("config.toml:3: expected '=', found EOF")
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			// LoadUserConfig's own contract: default config PLUS a non-nil
			// error, not a nil config.
			return &session.UserConfig{}, loadErr
		}

		value, warn := sendTransportFromConfig()
		if value != "tmux" {
			t.Errorf("value = %q, want %q (conservative on a load error)", value, "tmux")
		}
		if warn == "" {
			t.Fatal("expected a non-empty warning on a load error")
		}
		if !strings.Contains(warn, loadErr.Error()) {
			t.Errorf("warning %q does not mention the underlying error %q", warn, loadErr)
		}
		if !strings.Contains(warn, "tmux") {
			t.Errorf("warning %q should say it's using the tmux transport", warn)
		}
	})

	t.Run("clean load, explicit tmux pin", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			return &session.UserConfig{SendTransport: "tmux"}, nil
		}

		value, warn := sendTransportFromConfig()
		if value != "tmux" {
			t.Errorf("value = %q, want %q", value, "tmux")
		}
		if warn != "" {
			t.Errorf("warn = %q, want empty on a clean load", warn)
		}
	})

	t.Run("clean load, no key -> tmux (socket is opt-in)", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			return &session.UserConfig{}, nil
		}

		value, warn := sendTransportFromConfig()
		if value != "tmux" {
			t.Errorf("value = %q, want %q", value, "tmux")
		}
		if warn != "" {
			t.Errorf("warn = %q, want empty on a clean load", warn)
		}
	})

	t.Run("clean load, explicit auto opt-in", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			return &session.UserConfig{SendTransport: "auto"}, nil
		}

		value, warn := sendTransportFromConfig()
		if value != "auto" {
			t.Errorf("value = %q, want %q", value, "auto")
		}
		if warn != "" {
			t.Errorf("warn = %q, want empty on a clean load", warn)
		}
	})

	t.Run("unknown value -> tmux, with a warning naming it", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			return &session.UserConfig{SendTransport: "AUTO"}, nil
		}

		value, warn := sendTransportFromConfig()
		if value != "tmux" {
			t.Errorf("value = %q, want %q", value, "tmux")
		}
		if !strings.Contains(warn, "AUTO") {
			t.Errorf("warning %q should name the unrecognized value", warn)
		}
		if !strings.Contains(warn, "tmux") {
			t.Errorf("warning %q should say it's using the tmux transport", warn)
		}
	})

	t.Run("nil config, no error -> tmux, no warning", func(t *testing.T) {
		orig := loadUserConfigForSend
		t.Cleanup(func() { loadUserConfigForSend = orig })
		loadUserConfigForSend = func() (*session.UserConfig, error) {
			return nil, nil
		}

		value, warn := sendTransportFromConfig()
		if value != "tmux" {
			t.Errorf("value = %q, want %q", value, "tmux")
		}
		if warn != "" {
			t.Errorf("warn = %q, want empty", warn)
		}
	})
}

func TestIsBareSlashCommand(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"/compact", true},
		{"  /help", true},
		{"\t/rename foo", true},
		{"", false},
		{"   ", false},
		{"not a slash command", false},
		{"regular message", false},
	}
	for _, tc := range cases {
		if got := isBareSlashCommand(tc.msg); got != tc.want {
			t.Errorf("isBareSlashCommand(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// writeSessionRecord drops a Claude sessions/<pid>.json under claudeDir, the
// on-disk shape resolveClaudeSocketTargetForInstance reads.
func writeSessionRecord(t *testing.T, claudeDir string, pid int, sessionID string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + strconv.Itoa(pid) + `,"sessionId":"` + sessionID +
		`","updatedAt":1000,"procStart":"x","peerProtocol":1,"messagingSocketPath":"/tmp/x.sock"}`
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveClaudeSocketTargetForInstance_UsesInstanceConfigDir is the
// #2100 correction: the resolver must read the dir the instance runs under,
// never $HOME/.claude. The two halves discriminate: a record present ONLY in
// the instance's dir gets past the record lookup (failing later, at the pane
// probe, which has no tmux session in a test), while a record present ONLY
// in $HOME/.claude is never seen at all.
func TestResolveClaudeSocketTargetForInstance_UsesInstanceConfigDir(t *testing.T) {
	newInst := func() *session.Instance {
		return &session.Instance{ID: "i1", Title: "target", Tool: "claude", ClaudeSessionID: "sid-1"}
	}

	t.Run("record in the instance's dir is found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		instDir := filepath.Join(t.TempDir(), "account-claude")
		t.Setenv("CLAUDE_CONFIG_DIR", instDir)
		session.ClearUserConfigCache()
		t.Cleanup(session.ClearUserConfigCache)
		writeSessionRecord(t, instDir, 4242, "sid-1")

		_, err := resolveClaudeSocketTargetForInstance(newInst())
		var unavail *send.Unavailable
		if !errors.As(err, &unavail) {
			t.Fatalf("err = %v, want *send.Unavailable", err)
		}
		// Got past the record lookup: the failure is the pane probe, which
		// cannot run without a live tmux session.
		if unavail.Reason != send.ReasonNotInPaneTree {
			t.Errorf("reason = %q, want %q (the record in the instance's dir was found)", unavail.Reason, send.ReasonNotInPaneTree)
		}
	})

	t.Run("record only under $HOME/.claude is not used", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		instDir := filepath.Join(t.TempDir(), "account-claude")
		t.Setenv("CLAUDE_CONFIG_DIR", instDir)
		session.ClearUserConfigCache()
		t.Cleanup(session.ClearUserConfigCache)
		writeSessionRecord(t, filepath.Join(home, ".claude"), 4242, "sid-1")

		_, err := resolveClaudeSocketTargetForInstance(newInst())
		var unavail *send.Unavailable
		if !errors.As(err, &unavail) {
			t.Fatalf("err = %v, want *send.Unavailable", err)
		}
		if unavail.Reason != send.ReasonNoRecord {
			t.Errorf("reason = %q, want %q ($HOME/.claude must not be consulted)", unavail.Reason, send.ReasonNoRecord)
		}
	})

	t.Run("the account layer beats the env var", func(t *testing.T) {
		// The env rung alone does not prove the resolver walks the real
		// chain — an account-scoped session is the case #2100 is about.
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		envDir := filepath.Join(t.TempDir(), "env-claude")
		accountDir := filepath.Join(t.TempDir(), "work-account-claude")
		t.Setenv("CLAUDE_CONFIG_DIR", envDir)

		if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := session.SaveUserConfig(&session.UserConfig{
			Profiles: map[string]session.ProfileSettings{
				"work": {Claude: session.ProfileClaudeSettings{ConfigDir: accountDir}},
			},
		}); err != nil {
			t.Fatalf("SaveUserConfig: %v", err)
		}
		session.ClearUserConfigCache()
		t.Cleanup(session.ClearUserConfigCache)

		// Record only under the ACCOUNT's dir; a decoy under the env dir.
		writeSessionRecord(t, accountDir, 4242, "sid-1")
		writeSessionRecord(t, envDir, 5353, "sid-1")

		inst := newInst()
		inst.Account = "work"
		_, err := resolveClaudeSocketTargetForInstance(inst)
		var unavail *send.Unavailable
		if !errors.As(err, &unavail) {
			t.Fatalf("err = %v, want *send.Unavailable", err)
		}
		// Got past the record lookup using the account's dir; the failure
		// is the pane probe, which has no live tmux session in a test.
		if unavail.Reason != send.ReasonNotInPaneTree {
			t.Errorf("reason = %q, want %q (the account's dir must be scanned)", unavail.Reason, send.ReasonNotInPaneTree)
		}
	})

	t.Run("an account with no record is not rescued by the env dir", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		envDir := filepath.Join(t.TempDir(), "env-claude")
		accountDir := filepath.Join(t.TempDir(), "work-account-claude")
		t.Setenv("CLAUDE_CONFIG_DIR", envDir)

		if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := session.SaveUserConfig(&session.UserConfig{
			Profiles: map[string]session.ProfileSettings{
				"work": {Claude: session.ProfileClaudeSettings{ConfigDir: accountDir}},
			},
		}); err != nil {
			t.Fatalf("SaveUserConfig: %v", err)
		}
		session.ClearUserConfigCache()
		t.Cleanup(session.ClearUserConfigCache)

		// Only the env dir has the record; the account's dir has none.
		writeSessionRecord(t, envDir, 5353, "sid-1")

		inst := newInst()
		inst.Account = "work"
		_, err := resolveClaudeSocketTargetForInstance(inst)
		var unavail *send.Unavailable
		if !errors.As(err, &unavail) || unavail.Reason != send.ReasonNoRecord {
			t.Errorf("err = %v, want *send.Unavailable(%s): another dir's record must not be used", err, send.ReasonNoRecord)
		}
	})

	t.Run("nil instance", func(t *testing.T) {
		_, err := resolveClaudeSocketTargetForInstance(nil)
		var unavail *send.Unavailable
		if !errors.As(err, &unavail) || unavail.Reason != send.ReasonNoRecord {
			t.Errorf("err = %v, want *send.Unavailable(%s)", err, send.ReasonNoRecord)
		}
	})
}

// TestRunTmuxSend_SurfacesRecordSelectionReasons: the two #2100 selection
// refusals are genuine "a socket was attempted and refused" reasons, so they
// must reach the --json fallback_reason field rather than being filtered out
// as selector-level reasons the way an explicit pin is.
func TestRunTmuxSend_SurfacesRecordSelectionReasons(t *testing.T) {
	for _, reason := range []send.UnavailableReason{send.ReasonNotInPaneTree, send.ReasonAmbiguousRecord} {
		t.Run(string(reason), func(t *testing.T) {
			if selectorLevelReasons[reason] {
				t.Fatalf("%s must not be a selector-level reason: a socket WAS attempted", reason)
			}
			mock := &mockSendRetryTarget{statuses: []string{"active"}, panes: []string{""}}
			res, err := performSend(claudeInst("sid"), mock, "hello", false, defaultSendTuning(), "auto", false, nil,
				func(*session.Instance) (send.ClaudeSocketTarget, error) {
					return send.ClaudeSocketTarget{}, &send.Unavailable{Reason: reason}
				}, nil)
			if err != nil {
				t.Fatalf("performSend: %v", err)
			}
			if res.transport != "tmux" {
				t.Errorf("transport = %q, want tmux (a pre-write refusal falls back)", res.transport)
			}
			if res.fallbackReason != reason {
				t.Errorf("fallbackReason = %q, want %q", res.fallbackReason, reason)
			}
			if got := res.jsonFields()["fallback_reason"]; got != string(reason) {
				t.Errorf("fallback_reason = %v, want %q", got, reason)
			}
		})
	}
}
