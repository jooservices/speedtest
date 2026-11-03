package installer

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

//go:embed install_remote.sh
var installRemoteScript string

//go:embed rollback_remote.sh
var rollbackRemoteScript string

//go:embed commit_remote.sh
var commitRemoteScript string

//go:embed uninstall_remote.sh
var uninstallRemoteScript string

type progressFunc func(stage string, pct float64)

func sudoPassword(input SSHInput) string {
	if input.SudoPassword != "" {
		return input.SudoPassword
	}
	if input.Auth == "password" {
		return input.Secret
	}
	return ""
}

func sudoPreflight(client *ssh.Client, pass string) error {
	err := sshRun(client, "sudo -S -p '' /bin/sh -c 'true'", pass, nil)
	if err != nil {
		return ErrSudo
	}
	return nil
}

func uploadConfig(client *ssh.Client, id string, centerURL string, nodeName string, pass string) error {
	configPath := configPath(id)
	payload := []byte("INSTALL_ID=" + id + "\nCENTER_URL_B64=" + encode(centerURL) + "\nNODE_NAME_B64=" + encode(nodeName) + "\n")
	command := "sudo -S -p '' /bin/sh -c 'umask 077; mkdir -m 0700 " + configDir(id) + "; cat > " + configPath + "'"
	return sshRun(client, command, pass, payload)
}

func runInstallScript(ctx context.Context, client *ssh.Client, id, pass, token string, onProgress progressFunc) (string, error) {
	command := "sudo -S -p '' /bin/sh -s -- " + configPath(id)
	stage := ""
	tokenSent := false
	var tokenErr error
	_, err := sshRunProgress(ctx, client, command, pass, []byte(installRemoteScript), func(next string, pct float64) {
		stage = next
		if onProgress != nil {
			onProgress(next, pct)
		}
		if next == "enroll" && !tokenSent {
			tokenSent = true
			tokenErr = writeTokenFIFO(client, id, pass, token)
		}
	})
	if err == nil && tokenErr != nil {
		err = tokenErr
	}
	if err != nil {
		if stage == "unsupported" {
			return stage, ErrUnsupported
		}
		if errors.Is(err, ErrDisconnect) {
			return stage, ErrDisconnect
		}
		return stage, ErrRemoteFailure
	}
	return stage, nil
}

func writeTokenFIFO(client *ssh.Client, id, pass, token string) error {
	path := configDir(id) + "/token.pipe"
	command := "sudo -S -p '' /bin/sh -c 'cat > " + path + "'"
	if err := sshRun(client, command, pass, []byte(token+"\n")); err != nil {
		return ErrDisconnect
	}
	return nil
}

func rollbackRemote(client *ssh.Client, id, pass string) error {
	command := "sudo -S -p '' /bin/sh -s -- " + id
	if err := sshRun(client, command, pass, []byte(rollbackRemoteScript)); err != nil {
		return ErrDisconnect
	}
	return nil
}

// runUninstallScript removes the agent with the fixed uninstall script and reports its stages.
func runUninstallScript(ctx context.Context, client *ssh.Client, pass string, onProgress progressFunc) (string, error) {
	return sshRunProgress(ctx, client, "sudo -S -p '' /bin/sh -s", pass, []byte(uninstallRemoteScript), onProgress)
}

func commitRemote(client *ssh.Client, id, pass string) error {
	command := "sudo -S -p '' /bin/sh -s -- " + id
	if err := sshRun(client, command, pass, []byte(commitRemoteScript)); err != nil {
		return ErrDisconnect
	}
	return nil
}

func sshRun(client *ssh.Client, command, pass string, payload []byte) error {
	_, err := sshRunProgress(context.Background(), client, command, pass, payload, nil)
	return err
}

func sshRunProgress(ctx context.Context, client *ssh.Client, command, pass string, payload []byte, onProgress progressFunc) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", ErrDisconnect
	}
	defer func() { _ = session.Close() }()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", ErrDisconnect
	}
	session.Stderr = io.Discard
	session.Stdin = bytes.NewReader(append(append([]byte(pass), '\n'), payload...))
	if err := session.Start(sudoCommand(command)); err != nil {
		return "", ErrDisconnect
	}
	stage := ""
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 4096)
	for scanner.Scan() {
		next, pct, ok := parseProgress(scanner.Text())
		if !ok {
			continue
		}
		stage = next
		if onProgress != nil {
			onProgress(next, pct)
		}
	}
	scanErr := scanner.Err()
	waitErr := session.Wait()
	if scanErr != nil || ctx.Err() != nil {
		return stage, ErrDisconnect
	}
	if waitErr != nil {
		var exitErr *ssh.ExitError
		if !errors.As(waitErr, &exitErr) {
			return stage, ErrDisconnect
		}
		return stage, ErrRemoteFailure
	}
	return stage, nil
}

func parseProgress(line string) (string, float64, bool) {
	parts := strings.Split(line, "|")
	if len(parts) != 3 || parts[0] != "STP" {
		return "", 0, false
	}
	pct, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 100 {
		return "", 0, false
	}
	switch parts[1] {
	case "detect", "download", "verify", "user", "enroll", "start", "prepared", "unsupported", "stop", "files", "account", "removed":
		return parts[1], pct, true
	default:
		return "", 0, false
	}
}

var ErrRemoteFailure = errors.New("remote installer step failed")

func configPath(id string) string { return configDir(id) + "/config.env" }

func configDir(id string) string { return "/var/tmp/speedtest-installer-" + id }

func encode(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func stageFailureMessage(stage string) string {
	switch stage {
	case "download":
		return "The node could not download the agent release from the command center."
	case "verify":
		return "The downloaded agent checksum did not match the release manifest."
	case "user":
		return "The installer could not create or prepare the dedicated agent account."
	case "enroll":
		return "The agent could not enroll with the command center."
	case "start", "prepared":
		return "The agent service could not be started."
	default:
		return "The remote installation failed. Check the node and retry."
	}
}

func messageForFailure(err error, stage string) string {
	switch {
	case errors.Is(err, ErrHostKeyChange):
		return "The SSH host key changed. Probe the node again and confirm its fingerprint."
	case errors.Is(err, ErrAuth):
		return "SSH authentication failed. Check the username and password or private key."
	case errors.Is(err, ErrSudo):
		return "The SSH account must be allowed to run sudo on the node."
	case errors.Is(err, ErrUnsupported):
		return "This node is not supported. Use Linux on amd64 or arm64."
	case errors.Is(err, ErrDisconnect):
		return "The SSH connection was lost. Check the node and retry."
	default:
		return stageFailureMessage(stage)
	}
}

func secretInputError(input InstallInput) error {
	if len(input.SSH.Secret) == 0 {
		return ErrInvalid
	}
	if input.SSH.Auth == "password" && strings.ContainsAny(input.SSH.Secret, "\r\n") && input.SSH.SudoPassword == "" {
		return fmt.Errorf("%w: password cannot be used for sudo", ErrInvalid)
	}
	return nil
}

// sudoCommand consumes the password separately from the command payload. Sudo
// may consume no input with cached authentication or NOPASSWD permissions.
func sudoCommand(command string) string {
	return "IFS= read -r sudo_password; printf '%s\n' \"$sudo_password\" | sudo -S -p '' -v >/dev/null 2>&1 || exit 1; unset sudo_password; exec " + strings.Replace(command, "sudo -S -p '' ", "sudo -n ", 1)
}
