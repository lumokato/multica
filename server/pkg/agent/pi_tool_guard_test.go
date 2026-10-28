package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPiToolGuardExtensionDefaultsBashTimeout(t *testing.T) {
	t.Setenv("MULTICA_PI_BASH_TIMEOUT", "")
	backend := &piBackend{
		cfg:           Config{BuiltinRuntime: true, Logger: slog.Default()},
		providerLabel: "pi",
	}
	path, err := backend.preparePiToolGuardExtension()
	if err != nil {
		t.Fatalf("prepare tool guard: %v", err)
	}
	t.Cleanup(func() { removePiToolGuardExtension(path) })
	if path == "" {
		t.Fatal("built-in Pi did not get a tool guard extension")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tool guard: %v", err)
	}
	if !strings.Contains(string(data), "DEFAULT_TIMEOUT_SECONDS = 600") {
		t.Fatalf("default timeout missing from guard:\n%s", data)
	}
}

func TestPiToolGuardExtensionHonorsEnv(t *testing.T) {
	cases := []struct {
		env  string
		want string // empty means no extension
	}{
		{env: "45s", want: "DEFAULT_TIMEOUT_SECONDS = 45"},
		{env: "120", want: "DEFAULT_TIMEOUT_SECONDS = 120"},
		{env: "off", want: ""},
		{env: "0", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("MULTICA_PI_BASH_TIMEOUT", tc.env)
			backend := &piBackend{
				cfg:           Config{BuiltinRuntime: true, Logger: slog.Default()},
				providerLabel: "pi",
			}
			path, err := backend.preparePiToolGuardExtension()
			if err != nil {
				t.Fatalf("prepare tool guard: %v", err)
			}
			t.Cleanup(func() { removePiToolGuardExtension(path) })
			if tc.want == "" {
				if path != "" {
					t.Fatalf("env %q produced an extension at %s, want none", tc.env, path)
				}
				return
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read tool guard: %v", err)
			}
			if !strings.Contains(string(data), tc.want) {
				t.Fatalf("env %q: guard %q missing %q", tc.env, data, tc.want)
			}
		})
	}
}

func TestPiToolGuardExtensionScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		label   string
		builtin bool
	}{
		{name: "omp", label: "omp", builtin: true},
		{name: "custom-runtime-profile", label: "pi", builtin: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &piBackend{
				cfg:           Config{BuiltinRuntime: tc.builtin, Logger: slog.Default()},
				providerLabel: tc.label,
			}
			path, err := backend.preparePiToolGuardExtension()
			if err != nil {
				t.Fatalf("prepare tool guard: %v", err)
			}
			if path != "" {
				t.Fatalf("%s got a guard extension; scope should be built-in Pi only", tc.name)
			}
		})
	}
}

// TestPiToolGuardExtensionBehaviour executes the generated extension with Node
// and a mock ExtensionAPI. It proves the handler only fills a missing bash
// timeout and leaves explicit budgets and other tools alone.
func TestPiToolGuardExtensionBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	t.Setenv("MULTICA_PI_BASH_TIMEOUT", "90")
	backend := &piBackend{
		cfg:           Config{BuiltinRuntime: true, Logger: slog.Default()},
		providerLabel: "pi",
	}
	path, err := backend.preparePiToolGuardExtension()
	if err != nil {
		t.Fatalf("prepare tool guard: %v", err)
	}
	t.Cleanup(func() { removePiToolGuardExtension(path) })

	driver := filepath.Join(t.TempDir(), "driver.mjs")
	script := `import extension from ` + strconv.Quote(path) + `;
let handler;
const pi = { on: (name, fn) => { if (name === "tool_call") handler = fn; } };
extension(pi);
if (!handler) throw new Error("tool_call handler not registered");
const missing = { toolName: "bash", input: { command: "sleep 600" } };
handler(missing);
const explicit = { toolName: "bash", input: { command: "make test", timeout: 1800 } };
handler(explicit);
const other = { toolName: "read", input: { path: "x" } };
handler(other);
if (missing.input.timeout !== 90) throw new Error("missing timeout = " + missing.input.timeout);
if (explicit.input.timeout !== 1800) throw new Error("explicit timeout overwritten: " + explicit.input.timeout);
if (other.input.timeout !== undefined) throw new Error("non-bash input mutated");
console.log("ok");
`
	if err := os.WriteFile(driver, []byte(script), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	cmd := exec.Command(node, driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("extension behaviour: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("driver output = %q, want ok", out)
	}
}

func TestBuildPiArgsAppendsToolGuardExtension(t *testing.T) {
	path := "/tmp/multica-pi-tool-guard-test/tool-timeout.mjs"
	args := buildPiArgs("/tmp/s.jsonl", ExecOptions{piToolGuardExtensionPath: path}, slog.Default())
	if len(args) < 2 || args[len(args)-2] != "--extension" || args[len(args)-1] != path {
		t.Fatalf("args = %v, want --extension %s at the end", args, path)
	}
}

func TestPiTerminateGraceDefault(t *testing.T) {
	previous := piTerminateGraceNanos.Swap(0)
	t.Cleanup(func() { piTerminateGraceNanos.Store(previous) })
	if got := piTerminateGrace(); got != 5*time.Second {
		t.Fatalf("piTerminateGrace = %s, want 5s", got)
	}
}

// TestPiExecuteAddsToolGuardExtension proves the built-in Pi launch carries the
// generated extension through the real argv path, not only buildPiArgs in
// isolation. The fake still reports a normal completion so the session closes.
func TestPiExecuteAddsToolGuardExtension(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	t.Setenv("MULTICA_PI_BASH_TIMEOUT", "30")
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	fakePath := filepath.Join(dir, "pi")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"cat > /dev/null\n" +
		"printf '%s\\n' '{\"type\":\"agent_start\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input\":1,\"output\":1,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":2}}}'\n" +
		"exit 0\n"
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("pi", Config{ExecutablePath: fakePath, BuiltinRuntime: true, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	session, err := backend.Execute(context.Background(), "prompt-ignored", ExecOptions{
		ResumeSessionID: filepath.Join(dir, "session.jsonl"),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	result := waitPiResult(t, session, 5*time.Second)
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error=%q)", result.Status, result.Error)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read fake pi args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var guardPath string
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == "--extension" && strings.Contains(lines[i+1], piToolGuardExtensionDirPrefix) {
			guardPath = lines[i+1]
		}
	}
	if guardPath == "" {
		t.Fatalf("args = %v, want a %s extension", lines, piToolGuardExtensionDirPrefix)
	}
	scriptData, err := os.ReadFile(guardPath)
	if err != nil {
		// The backend removes the temp directory after the run; the path in argv
		// is only expected to have existed while the process ran, so a missing
		// file here is not a failure on its own.
		return
	}
	if !strings.Contains(string(scriptData), "DEFAULT_TIMEOUT_SECONDS = 30") {
		t.Fatalf("guard path %s did not contain the configured timeout:\n%s", guardPath, scriptData)
	}
}

func TestPiBashTimeoutSecondsParsing(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		enabled bool
	}{
		{raw: "", want: DefaultPiBashTimeoutSeconds, enabled: true},
		{raw: "600", want: 600, enabled: true},
		{raw: "45s", want: 45, enabled: true},
		{raw: "1h", want: 3600, enabled: true},
		{raw: "500ms", want: 1, enabled: true},
		{raw: "1500ms", want: 2, enabled: true},
		{raw: "0", want: 0, enabled: false},
		{raw: "off", want: 0, enabled: false},
		{raw: "DISABLED", want: 0, enabled: false},
		{raw: "none", want: 0, enabled: false},
		{raw: "0s", want: DefaultPiBashTimeoutSeconds, enabled: true},
		{raw: "-5", want: DefaultPiBashTimeoutSeconds, enabled: true},
		{raw: "-5s", want: DefaultPiBashTimeoutSeconds, enabled: true},
		{raw: "not-a-duration", want: DefaultPiBashTimeoutSeconds, enabled: true},
		{raw: "3000000s", want: maxPiBashTimeoutSeconds, enabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("MULTICA_PI_BASH_TIMEOUT", tc.raw)
			got, enabled := piBashTimeoutSeconds(slog.Default())
			if got != tc.want || enabled != tc.enabled {
				t.Fatalf("piBashTimeoutSeconds(%q) = (%d, %v), want (%d, %v)", tc.raw, got, enabled, tc.want, tc.enabled)
			}
		})
	}
}

func TestPiToolGuardExtensionPositivelyBoundsSubSecond(t *testing.T) {
	t.Setenv("MULTICA_PI_BASH_TIMEOUT", "500ms")
	backend := &piBackend{
		cfg:           Config{BuiltinRuntime: true, Logger: slog.Default()},
		providerLabel: "pi",
	}
	path, err := backend.preparePiToolGuardExtension()
	if err != nil {
		t.Fatalf("prepare tool guard: %v", err)
	}
	t.Cleanup(func() { removePiToolGuardExtension(path) })
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tool guard: %v", err)
	}
	if !strings.Contains(string(data), "DEFAULT_TIMEOUT_SECONDS = 1") {
		t.Fatalf("500ms must become a positive 1s timeout, got:\n%s", data)
	}
}

// TestPiToolGuardChainWithRealPi runs the generated extension through the real
// installed Pi extension loader, tool_call hook and bash tool. It is gated
// because it depends on an external Pi installation; no model/provider is
// contacted. Run with:
//
//	MULTICA_RUN_PI_TOOLCHAIN_SMOKE=1 go test ./pkg/agent -run TestPiToolGuardChainWithRealPi -count=1 -v
func TestPiToolGuardChainWithRealPi(t *testing.T) {
	if os.Getenv("MULTICA_RUN_PI_TOOLCHAIN_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_PI_TOOLCHAIN_SMOKE=1 to run the installed-Pi chain")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	root := os.Getenv("PI_CODING_AGENT_ROOT")
	if root == "" {
		root = "/usr/local/lib/node_modules/@mariozechner/pi-coding-agent"
	}
	if _, err := os.Stat(filepath.Join(root, "dist/core/extensions/loader.js")); err != nil {
		t.Skipf("installed Pi not found at %s: %v", root, err)
	}

	t.Setenv("MULTICA_PI_BASH_TIMEOUT", "1s")
	backend := &piBackend{
		cfg:           Config{BuiltinRuntime: true, Logger: slog.Default()},
		providerLabel: "pi",
	}
	guardPath, err := backend.preparePiToolGuardExtension()
	if err != nil {
		t.Fatalf("prepare tool guard: %v", err)
	}
	t.Cleanup(func() { removePiToolGuardExtension(guardPath) })

	script, err := filepath.Abs("testdata/pi_tool_guard_chain.mjs")
	if err != nil {
		t.Fatalf("resolve chain script: %v", err)
	}
	cmd := exec.Command(node, script, guardPath)
	cmd.Env = append(os.Environ(), "PI_CODING_AGENT_ROOT="+root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Pi tool chain failed: %v\n%s", err, out)
	}
	t.Logf("real Pi tool chain:\n%s", out)
}
