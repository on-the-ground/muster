package worker

import (
	"os"
	"sort"
	"strings"

	"github.com/on-the-ground/muster/internal/contract"
)

// Only explicit operator-selected names reach a worker. Transport trust and
// proxy settings are shared prerequisites, not unrestricted parent credentials.
func workerEnvironment(kind string, extra []string) []string {
	names := []string{"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TMP", "TEMP", "USER", "SHELL", "SYSTEMROOT", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_EXEC_SERVER_PROXY_PRIVATE_IPS_VIA_UPSTREAM"}
	if kind == "claude" {
		names = append(names, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR")
	}
	if kind == "codex" {
		names = append(names, "CODEX_HOME", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID")
	}
	names = append(names, extra...)
	values := map[string]string{}
	for _, name := range names {
		if contract.ForbiddenEnvironment(name) {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	if kind == "claude" {
		// Safe mode retains subscription/OAuth authentication. Explicitly disable
		// both CLAUDE.md discovery and auto memory as defense in depth.
		values["CLAUDE_CODE_DISABLE_CLAUDE_MDS"] = "1"
		values["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] = "1"
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}

func envValue(env []string, name string) string {
	for _, item := range env {
		if k, v, ok := strings.Cut(item, "="); ok && k == name {
			return v
		}
	}
	return ""
}
