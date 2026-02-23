// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gitauth

import (
	"os"
	"os/exec"
	"strings"
)

// DisableInteractivePrompts enforces a non-interactive git authentication policy
// for the current process by overriding environment variables.
//
// This prevents git / ssh from prompting for credentials (stdin / askpass), so
// operations fail fast when credentials are unavailable.
func DisableInteractivePrompts() {
	// For HTTPS remotes, disable username/password prompting.
	_ = os.Setenv("GIT_TERMINAL_PROMPT", "0")
	// Disable interactive Git Credential Manager flows (where installed).
	_ = os.Setenv("GCM_INTERACTIVE", "Never")
	// For SSH remotes, prevent passphrase/password prompting by appending
	// -o BatchMode=yes to the SSH command. We preserve any existing SSH
	// command from the environment or git config so that platform-specific
	// SSH binaries (e.g. Windows OpenSSH) continue to be used.
	_ = os.Setenv("GIT_SSH_COMMAND", resolveSSHCommand()+" -o BatchMode=yes")
}

// resolveSSHCommand returns the base SSH command that git would use, checking
// (in order): $GIT_SSH_COMMAND, git config core.sshCommand, $GIT_SSH, and
// falling back to "ssh".
func resolveSSHCommand() string {
	// 1. GIT_SSH_COMMAND env var (highest priority in git itself).
	if v := os.Getenv("GIT_SSH_COMMAND"); v != "" {
		return v
	}

	// 2. git config core.sshCommand (global/system level).
	if cmd := gitConfigSSHCommand(); cmd != "" {
		return cmd
	}

	// 3. GIT_SSH env var (path to an SSH binary).
	if v := os.Getenv("GIT_SSH"); v != "" {
		return v
	}

	return "ssh"
}

// gitConfigSSHCommand queries git for core.sshCommand at the global/system level.
func gitConfigSSHCommand() string {
	p, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	out, err := exec.Command(p, "config", "--global", "--includes", "core.sshCommand").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
