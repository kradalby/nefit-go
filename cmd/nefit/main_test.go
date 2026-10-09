package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain keeps the developer's credentials out of in-process tests: the
// flag globals are initialised from the environment.
func TestMain(m *testing.M) {
	if os.Getenv("NEFIT_CLI_TEST_PROCESS") != "1" {
		*serialNumber, *accessKey, *password = "SYNTHETIC_SERIAL", "SYNTHETIC_ACCESS_KEY", "SYNTHETIC_PASSWORD"
	}
	os.Exit(m.Run())
}

func TestCLIProcess(t *testing.T) {
	if os.Getenv("NEFIT_CLI_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"nefit"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI argument separator")
}

// cliCommand re-executes the test binary as the CLI with synthetic credentials.
func cliCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "NEFIT_") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env,
		"NEFIT_CLI_TEST_PROCESS=1",
		"NEFIT_SERIAL_NUMBER=SYNTHETIC_SERIAL_SENTINEL",
		"NEFIT_ACCESS_KEY=SYNTHETIC_ACCESS_KEY_SENTINEL",
		"NEFIT_PASSWORD=SYNTHETIC_PASSWORD_SENTINEL",
	)
	return cmd
}

func runCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, err := cliCommand(ctx, args...).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI did not return: %v", ctx.Err())
	}
	if err == nil {
		return string(output), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return string(output), exitErr.ExitCode()
}

func TestCLIUsageDoesNotDiscloseEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"help", []string{"--help"}, 0, "-access-key"},
		{"help after explicit flags", []string{"--password=SYNTHETIC_PASSWORD_SENTINEL", "--access-key=SYNTHETIC_ACCESS_KEY_SENTINEL", "--help"}, 0, "-access-key"},
		{"missing command", nil, 2, "-access-key"},
		{"unknown command", []string{"unknown-command"}, 2, `unknown command "unknown-command"`},
		{"unknown set command", []string{"set", "unknown-command"}, 2, "nefit set <subcommand>"},
		{"bad flag", []string{"--unknown-flag"}, 2, "-access-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output, code := runCLI(t, tc.args...)
			if code != tc.code || !strings.Contains(output, "USAGE") || !strings.Contains(output, tc.want) {
				t.Fatalf("exit %d, want %d with usage and %q: %s", code, tc.code, tc.want, output)
			}
			for _, sentinel := range []string{"SYNTHETIC_SERIAL_SENTINEL", "SYNTHETIC_ACCESS_KEY_SENTINEL", "SYNTHETIC_PASSWORD_SENTINEL"} {
				if strings.Contains(output, sentinel) {
					t.Errorf("usage disclosed %s", sentinel)
				}
			}
		})
	}
}

func TestEnvironmentFlagPrecedence(t *testing.T) {
	for _, credential := range []struct{ name, env string }{
		{"serial", "NEFIT_SERIAL_NUMBER"},
		{"access-key", "NEFIT_ACCESS_KEY"},
		{"password", "NEFIT_PASSWORD"},
	} {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"environment", nil, "SYNTHETIC_ENV_SENTINEL"},
			{"explicit", []string{"--" + credential.name, "SYNTHETIC_FLAG_SENTINEL"}, "SYNTHETIC_FLAG_SENTINEL"},
			{"empty equals", []string{"--" + credential.name + "="}, ""},
			{"empty argument", []string{"--" + credential.name, ""}, ""},
			{"last flag wins", []string{"--" + credential.name + "=first", "--" + credential.name + "="}, ""},
		} {
			t.Run(credential.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv(credential.env, "SYNTHETIC_ENV_SENTINEL")
				fs := flag.NewFlagSet("test", flag.ContinueOnError)
				value := envStringFlag(fs, credential.name, credential.env, "credential")
				if err := fs.Parse(tc.args); err != nil {
					t.Fatal(err)
				}
				if *value != tc.want {
					t.Fatalf("value %q, want %q", *value, tc.want)
				}
			})
		}
	}
}

func TestCreateClientRequiresCredentials(t *testing.T) {
	for _, credential := range []struct {
		name  string
		value *string
	}{{"serial", serialNumber}, {"access-key", accessKey}, {"password", password}} {
		t.Run(credential.name, func(t *testing.T) {
			saved := []string{*serialNumber, *accessKey, *password}
			t.Cleanup(func() { *serialNumber, *accessKey, *password = saved[0], saved[1], saved[2] })
			*serialNumber, *accessKey, *password = "serial", "key", "secret"
			*credential.value = ""
			if _, err := createClient(); err == nil || !strings.Contains(err.Error(), "required (--"+credential.name) {
				t.Fatalf("empty %s accepted: %v", credential.name, err)
			}
		})
	}
}

func TestServeStartupExitCodes(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	serve := []string{"serve", "--device-ip=127.0.0.1", "--xmpp-listen=127.0.0.1:0", "--http-listen=127.0.0.1:0"}
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"extra argument", append(serve, "extra"), 2, "unexpected serve arguments"},
		{"bad device IP", []string{"serve", "--device-ip=not-an-ip"}, 78, "--device-ip"},
		{"bad mode", append(serve, "--mode=bogus"), 78, "bogus"},
		{"unknown port name", append(serve, "--http-listen=127.0.0.1:htpp"), 78, "--http-listen"},
		{"upstream offline", append(serve, "--upstream=cloud.example:5222"), 78, "upstream address requires"},
		{"DNS answer elsewhere", append(serve, "--dns-listen=127.0.0.1:0", "--dns-address=192.0.2.1"), 78, "DNS address"},
		{"bad allowed host", append(serve, "--http-allowed-hosts=bad/host"), 78, "--http-allowed-hosts"},
		{"missing credentials", append([]string{"--serial="}, serve...), 78, "serial number required"},
		{"zero timeout", append([]string{"--timeout=0"}, serve...), 78, "--timeout"},
		{"malformed listen address", append(serve, "--http-listen=bad"), 78, "missing port"},
		{"empty HTTP listen address", append(serve, "--http-listen="), 78, "--http-listen"},
		// A supervisor must retry these: the port or address may come later.
		{"port in use", append(serve, "--xmpp-listen="+held.Addr().String()), 1, "address already in use"},
		{"HTTP port in use", append(serve, "--http-listen="+held.Addr().String()), 1, "address already in use"},
		{"address not assigned", append(serve, "--xmpp-listen=192.0.2.1:0"), 1, syscall.EADDRNOTAVAIL.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			output, code := runCLI(t, tc.args...)
			if code != tc.code || !strings.Contains(output, tc.want) {
				t.Fatalf("exit %d, want %d and %q: %s", code, tc.code, tc.want, output)
			}
		})
	}
}

func TestStartupErrorPermission(t *testing.T) {
	// A port the user may not bind needs a configuration change, not a restart.
	err := startupError(&net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EACCES)})
	var config configError
	if !errors.As(err, &config) {
		t.Fatal("permission denied left to restarts:", err)
	}
}
