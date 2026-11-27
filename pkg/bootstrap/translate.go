/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package bootstrap translates Cluster API kubeadm bootstrap data
// (CABPK cloud-config) into a bash script for Verda's startup-script
// mechanism, which executes exactly one thing: a shell script, run
// as root at the end of the instance's cloud-init. Verda wraps
// whatever it is given in its own cloud-config and runs it with
// bash, so a CABPK document delivered raw would be executed as
// shell and fail; the translation below is what makes the upstream
// bootstrap data usable on Verda at all. The behavior this targets
// was verified empirically on Nov 26, 2026.
//
// The translation is deliberately mechanical, following the CABPK
// output contract (see the take-home research notes,
// cabpk-cloudconfig-shape.md): strip the cloud-init headers, run
// bootcmd, write files byte-exact (decoding per entry), ensure
// users, apply the NTP servers to systemd-timesyncd, then run the
// runcmd sequence in its exact order — including the kubeadm line's
// "&& echo success > /run/cluster-api/bootstrap-success.complete"
// sentinel chain, which is how Cluster API detects a successful
// bootstrap and must only fire when kubeadm itself succeeds.
//
// Two cloud-init sections are refused rather than improvised:
// disk_setup and fs_setup describe partitioning and filesystem
// creation in cloud-init's own DSL, and a wrong guess there is
// destructive. KubeadmConfigs almost never set them; if one does,
// Translate returns an error naming the section.
package bootstrap

import (
	"encoding/base64"
	"fmt"
	"path"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// cloudConfig is the subset of the cloud-config document CABPK
// emits. The key set is closed; anything else in the input is
// ignored the way cloud-init would ignore unknown keys.
type cloudConfig struct {
	WriteFiles []fileEntry    `yaml:"write_files"`
	BootCmd    []string       `yaml:"bootcmd"`
	RunCmd     []string       `yaml:"runcmd"`
	NTP        *ntpConfig     `yaml:"ntp"`
	Users      []userEntry    `yaml:"users"`
	DiskSetup  map[string]any `yaml:"disk_setup"`
	FSSetup    []any          `yaml:"fs_setup"`
	Mounts     [][]any        `yaml:"mounts"`
}

type fileEntry struct {
	Path        string          `yaml:"path"`
	Encoding    string          `yaml:"encoding"`
	Owner       string          `yaml:"owner"`
	Permissions filePermissions `yaml:"permissions"`
	Append      bool            `yaml:"append"`
	Content     string          `yaml:"content"`
}

type ntpConfig struct {
	Enabled bool     `yaml:"enabled"`
	Servers []string `yaml:"servers"`
}

type userEntry struct {
	Name              string   `yaml:"name"`
	Passwd            string   `yaml:"passwd"`
	Gecos             string   `yaml:"gecos"`
	Groups            string   `yaml:"groups"`
	Homedir           string   `yaml:"homedir"`
	Inactive          bool     `yaml:"inactive"`
	LockPasswd        bool     `yaml:"lock_passwd"`
	Shell             string   `yaml:"shell"`
	PrimaryGroup      string   `yaml:"primary_group"`
	Sudo              string   `yaml:"sudo"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
}

// filePermissions is the permissions value of a write_files entry.
// CABPK emits it as a quoted octal string ('0640'), and a plain
// string field handles that. The special handling exists for one
// failure mode: an unquoted 0640 parses in YAML as an integer, and
// decoding an integer into a string field fails the whole
// unmarshal. So: strings pass through unchanged; an integer with a
// leading zero (0640, 0o640) is parsed as the octal mode it denotes
// and formatted back to four-digit octal; any other integer keeps
// its literal text (chmod reads a three-digit mode fine).
type filePermissions string

func (p *filePermissions) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("permissions: expected a scalar, got kind %d", value.Kind)
	}
	if value.Tag == "!!int" && strings.HasPrefix(value.Value, "0") && len(value.Value) > 1 {
		n, err := strconv.ParseInt(value.Value, 0, 64)
		if err != nil {
			return fmt.Errorf("permissions: parsing %q: %w", value.Value, err)
		}
		*p = filePermissions(fmt.Sprintf("%04o", n))
		return nil
	}
	*p = filePermissions(value.Value)
	return nil
}

// Translate converts one CABPK cloud-config document into the bash
// script that reproduces it under Verda's startup-script mechanism.
func Translate(cloudConfigData []byte) (string, error) {
	if !looksLikeCloudConfig(cloudConfigData) {
		return "", fmt.Errorf("input does not look like a cloud-config document")
	}
	var cfg cloudConfig
	if err := yaml.Unmarshal(cloudConfigData, &cfg); err != nil {
		return "", fmt.Errorf("parsing cloud-config: %w", err)
	}
	if len(cfg.DiskSetup) > 0 || len(cfg.FSSetup) > 0 {
		return "", fmt.Errorf("cloud-config contains disk_setup/fs_setup, which have no safe mechanical bash translation; refusing to improvise partitioning")
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("# Generated by cluster-api-provider-verda from Cluster API kubeadm\n")
	b.WriteString("# bootstrap data (cloud-config). Do not edit on the instance.\n")
	b.WriteString("#\n")
	b.WriteString("# Deliberately no 'set -e': cloud-init runs every runcmd entry\n")
	b.WriteString("# regardless of individual failures, and the kubeadm entry's own\n")
	b.WriteString("# '&&' chain carries the success semantics (the bootstrap\n")
	b.WriteString("# sentinel is written only when kubeadm succeeds). Aborting the\n")
	b.WriteString("# whole script on the first failing pre-command would diverge\n")
	b.WriteString("# from the bootstrap contract.\n")

	emitBootCmd(&b, cfg.BootCmd)
	if err := emitFiles(&b, cfg.WriteFiles); err != nil {
		return "", err
	}
	emitUsers(&b, cfg.Users)
	emitNTP(&b, cfg.NTP)
	emitMounts(&b, cfg.Mounts)
	emitRunCmd(&b, cfg.RunCmd)

	return b.String(), nil
}

func looksLikeCloudConfig(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "##") {
			continue // the jinja template marker line
		}
		return line == "#cloud-config"
	}
	return false
}

func emitBootCmd(b *strings.Builder, cmds []string) {
	if len(cmds) == 0 {
		return
	}
	b.WriteString("\n# --- bootcmd ---\n")
	b.WriteString("# Note: cloud-init runs bootcmd on every boot; a startup script\n")
	b.WriteString("# runs once. CABPK boot commands are one-shot setup in practice.\n")
	for _, c := range cmds {
		b.WriteString(c + "\n")
	}
}

func emitFiles(b *strings.Builder, files []fileEntry) error {
	if len(files) == 0 {
		return nil
	}
	b.WriteString("\n# --- write_files ---\n")
	for i, f := range files {
		if f.Path == "" {
			return fmt.Errorf("write_files entry %d has no path", i)
		}
		fmt.Fprintf(b, "# %s\n", f.Path)
		if dir := path.Dir(f.Path); dir != "." && dir != "/" {
			fmt.Fprintf(b, "mkdir -p %s\n", shellQuote(dir))
		}
		redirect := ">"
		if f.Append {
			redirect = ">>"
		}
		switch f.Encoding {
		case "":
			if err := emitPlainFile(b, f, redirect, i); err != nil {
				return err
			}
		case "base64":
			delim := heredocDelimiter(i)
			fmt.Fprintf(b, "base64 -d %s %s <<'%s'\n%s\n%s\n", redirect, shellQuote(f.Path), delim, strings.TrimSpace(f.Content), delim)
		case "gzip":
			// The YAML scalar carries raw gzipped bytes; re-encode
			// them as base64 so the script stays text-safe.
			encoded := base64.StdEncoding.EncodeToString([]byte(f.Content))
			delim := heredocDelimiter(i)
			fmt.Fprintf(b, "base64 -d <<'%s' | gunzip -c %s %s\n%s\n%s\n", delim, redirect, shellQuote(f.Path), encoded, delim)
		case "gzip+base64":
			delim := heredocDelimiter(i)
			fmt.Fprintf(b, "base64 -d <<'%s' | gunzip -c %s %s\n%s\n%s\n", delim, redirect, shellQuote(f.Path), strings.TrimSpace(f.Content), delim)
		default:
			return fmt.Errorf("write_files entry %s: unsupported encoding %q", f.Path, f.Encoding)
		}
		if f.Owner != "" {
			fmt.Fprintf(b, "chown %s %s\n", shellQuote(f.Owner), shellQuote(f.Path))
		}
		if f.Permissions != "" {
			fmt.Fprintf(b, "chmod %s %s\n", shellQuote(string(f.Permissions)), shellQuote(f.Path))
		}
	}
	return nil
}

// emitPlainFile writes one unencoded file via a quoted heredoc, so
// the content lands byte-for-byte with no shell expansion. Content
// that cannot survive a heredoc (no trailing newline, or a line
// colliding with the delimiter) falls back to base64 embedding.
func emitPlainFile(b *strings.Builder, f fileEntry, redirect string, index int) error {
	delim := heredocDelimiter(index)
	containsDelim := false
	for _, line := range strings.Split(f.Content, "\n") {
		if line == delim {
			containsDelim = true
			break
		}
	}
	if !strings.HasSuffix(f.Content, "\n") || containsDelim {
		encoded := base64.StdEncoding.EncodeToString([]byte(f.Content))
		fmt.Fprintf(b, "base64 -d %s %s <<'%s'\n%s\n%s\n", redirect, shellQuote(f.Path), delim, encoded, delim)
		return nil
	}
	fmt.Fprintf(b, "cat %s %s <<'%s'\n%s%s\n", redirect, shellQuote(f.Path), delim, f.Content, delim)
	return nil
}

func emitUsers(b *strings.Builder, users []userEntry) {
	if len(users) == 0 {
		return
	}
	b.WriteString("\n# --- users ---\n")
	for _, u := range users {
		if u.Name == "" {
			continue
		}
		home := u.Homedir
		if home == "" {
			if u.Name == "root" {
				home = "/root"
			} else {
				home = "/home/" + u.Name
			}
		}
		if u.PrimaryGroup != "" {
			fmt.Fprintf(b, "getent group %s >/dev/null || groupadd %s\n", shellQuote(u.PrimaryGroup), shellQuote(u.PrimaryGroup))
		}
		var args []string
		args = append(args, "-m", "-d", home)
		if u.Shell != "" {
			args = append(args, "-s", u.Shell)
		}
		if u.Gecos != "" {
			args = append(args, "-c", shellQuote(u.Gecos))
		}
		if u.PrimaryGroup != "" {
			args = append(args, "-g", u.PrimaryGroup)
		}
		if u.Groups != "" {
			args = append(args, "-G", shellQuote(u.Groups))
		}
		fmt.Fprintf(b, "id %s >/dev/null 2>&1 || useradd %s %s\n", shellQuote(u.Name), strings.Join(args, " "), shellQuote(u.Name))
		if u.Passwd != "" {
			fmt.Fprintf(b, "usermod -p %s %s\n", shellQuote(u.Passwd), shellQuote(u.Name))
		}
		if u.LockPasswd {
			fmt.Fprintf(b, "usermod -L %s\n", shellQuote(u.Name))
		}
		if u.Inactive {
			fmt.Fprintf(b, "# untranslated: cloud-init 'inactive: true' for user %s has no sequenced equivalent here\n", u.Name)
		}
		if u.Sudo != "" {
			delim := "EOF_SUDOERS"
			fmt.Fprintf(b, "cat > %s <<'%s'\n%s %s\n%s\n", shellQuote("/etc/sudoers.d/"+u.Name), delim, u.Name, u.Sudo, delim)
			fmt.Fprintf(b, "chmod 0440 %s\n", shellQuote("/etc/sudoers.d/"+u.Name))
		}
		if len(u.SSHAuthorizedKeys) > 0 {
			sshDir := home + "/.ssh"
			fmt.Fprintf(b, "mkdir -p %s\n", shellQuote(sshDir))
			fmt.Fprintf(b, "chmod 0700 %s\n", shellQuote(sshDir))
			delim := "EOF_KEYS_" + u.Name
			fmt.Fprintf(b, "cat > %s <<'%s'\n", shellQuote(sshDir+"/authorized_keys"), delim)
			for _, key := range u.SSHAuthorizedKeys {
				b.WriteString(key + "\n")
			}
			b.WriteString(delim + "\n")
			fmt.Fprintf(b, "chmod 0600 %s\n", shellQuote(sshDir+"/authorized_keys"))
			fmt.Fprintf(b, "chown -R %s %s\n", shellQuote(u.Name), shellQuote(sshDir))
		}
	}
}

func emitNTP(b *strings.Builder, ntp *ntpConfig) {
	if ntp == nil || !ntp.Enabled || len(ntp.Servers) == 0 {
		return
	}
	b.WriteString("\n# --- ntp (systemd-timesyncd) ---\n")
	b.WriteString("mkdir -p /etc/systemd/timesyncd.conf.d\n")
	delim := "EOF_NTP"
	fmt.Fprintf(b, "cat > /etc/systemd/timesyncd.conf.d/99-capv.conf <<'%s'\n[Time]\nNTP=%s\n%s\n", delim, strings.Join(ntp.Servers, " "), delim)
	b.WriteString("systemctl restart systemd-timesyncd || true\n")
}

func emitMounts(b *strings.Builder, mounts [][]any) {
	if len(mounts) == 0 {
		return
	}
	b.WriteString("\n# --- mounts (fstab) ---\n")
	for _, row := range mounts {
		fields := make([]string, 0, len(row))
		for _, field := range row {
			fields = append(fields, fmt.Sprintf("%v", field))
		}
		fmt.Fprintf(b, "echo %s >> /etc/fstab\n", shellQuote(strings.Join(fields, " ")))
	}
	b.WriteString("mount -a || true\n")
}

func emitRunCmd(b *strings.Builder, cmds []string) {
	if len(cmds) == 0 {
		return
	}
	b.WriteString("\n# --- runcmd (exact order; the kubeadm entry's sentinel chain is preserved) ---\n")
	for _, c := range cmds {
		b.WriteString(c + "\n")
	}
}

func heredocDelimiter(index int) string {
	return "EOF_CAPV_" + strconv.Itoa(index)
}

// shellQuote single-quotes s for safe inclusion in a bash command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
