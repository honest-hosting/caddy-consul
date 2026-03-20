package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain runs all integration tests, then performs post-test assertions
// (like the 429 rate-limit check) that must run after everything else.
func TestMain(m *testing.M) {
	code := m.Run()

	// Post-test assertions — only run if tests were actually executed
	// (not skipped due to -run filter that excluded everything)
	if err := assertNoConsul429Errors(); err != nil {
		fmt.Fprintf(os.Stderr, "POST-TEST ASSERTION FAILED: %v\n", err)
		if code == 0 {
			code = 1
		}
	}

	os.Exit(code)
}

// assertNoConsul429Errors checks Caddy container logs for any Consul 429
// rate-limit errors. Any occurrence indicates the watcher or reconciler
// is making too many concurrent requests.
func assertNoConsul429Errors() error {
	out, err := exec.Command("docker", "logs", "caddy-consul-caddy-1").CombinedOutput()
	if err != nil {
		// If we can't read logs (e.g., container not running), skip the check
		return nil
	}

	logs := string(out)
	count := strings.Count(strings.ToLower(logs), "rate limit")

	if count > 0 {
		return fmt.Errorf(
			"expected zero Consul 429 rate-limit errors in Caddy logs, but found %d.\n"+
				"This indicates the watcher or reconciler is too aggressive with Consul API calls.\n"+
				"Relevant log lines:\n%s",
			count, extract429Lines(logs))
	}

	return nil
}

// extract429Lines returns log lines containing "429" for diagnostic output.
func extract429Lines(logs string) string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "429") {
			lines = append(lines, line)
			if len(lines) >= 10 {
				lines = append(lines, "... (truncated)")
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}
