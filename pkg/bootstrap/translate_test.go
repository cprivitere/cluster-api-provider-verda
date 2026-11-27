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

package bootstrap

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initFixture mirrors the shape CABPK emits for a control-plane
// init node, with absolute paths rewritten under root so the
// generated script can be executed safely in a test sandbox.
func initFixture(root string) string {
	return `## template: jinja
#cloud-config
write_files:
-   path: ` + root + `/run/kubeadm/kubeadm.yaml
    owner: root:root
    permissions: '0640'
    content: |
      apiVersion: kubeadm.k8s.io/v1beta4
      kind: ClusterConfiguration
      ---
      apiVersion: kubeadm.k8s.io/v1beta4
      kind: InitConfiguration
-   path: ` + root + `/etc/kubernetes/pki/ca.crt
    owner: root:root
    permissions: '0640'
    content: |
      -----BEGIN CERTIFICATE-----
      FAKE-CERT-FOR-TESTS
      -----END CERTIFICATE-----
-   path: ` + root + `/run/cluster-api/placeholder
    owner: root:root
    permissions: '0640'
    content: |
      This file is a placeholder.
runcmd:
-   'echo pre-kubeadm'
-   'kubeadm init --config ` + root + `/run/kubeadm/kubeadm.yaml --v=5 && echo success > ` + root +
		`/run/cluster-api/bootstrap-success.complete'
-   'echo post-kubeadm'
`
}

func TestTranslateInitShape(t *testing.T) {
	script, err := Translate([]byte(initFixture("/REALROOT")))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.HasPrefix(script, "#!/bin/bash\n") {
		t.Errorf("script does not start with a bash shebang:\n%s", script[:80])
	}
	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "set -e" || strings.HasPrefix(trimmed, "set -e ") || strings.HasPrefix(trimmed, "set -o errexit") {
			t.Errorf("script must not enable errexit (cloud-init runs every runcmd entry); found %q", trimmed)
		}
	}
	// The kubeadm entry and its sentinel chain survive verbatim.
	want := "kubeadm init --config /REALROOT/run/kubeadm/kubeadm.yaml --v=5 && echo success > " +
		"/REALROOT/run/cluster-api/bootstrap-success.complete"
	if !strings.Contains(script, want) {
		t.Errorf("kubeadm line not preserved verbatim; want %q in:\n%s", want, script)
	}
	// Files land before the commands that consume them, and the
	// runcmd order (pre, kubeadm, post) is exact.
	idxFile := strings.Index(script, "/REALROOT/run/kubeadm/kubeadm.yaml")
	idxKubeadm := strings.Index(script, "kubeadm init")
	idxPre := strings.Index(script, "echo pre-kubeadm")
	idxPost := strings.Index(script, "echo post-kubeadm")
	if idxFile >= idxPre || idxPre >= idxKubeadm || idxKubeadm >= idxPost {
		t.Errorf("ordering wrong: file=%d pre=%d kubeadm=%d post=%d\n%s", idxFile, idxPre, idxKubeadm, idxPost, script)
	}
	if !strings.Contains(script, "chmod '0640' '/REALROOT/etc/kubernetes/pki/ca.crt'") {
		t.Errorf("certificate permissions not applied:\n%s", script)
	}
}

// TestTranslateExecutesAndWritesExactFiles runs the generated
// script and checks the files it produces, byte for byte.
func TestTranslateExecutesAndWritesExactFiles(t *testing.T) {
	root := t.TempDir()
	script, err := Translate([]byte(initFixture(root)))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	// The fixture's runcmd invokes kubeadm, which does not exist
	// here; strip the runcmd section for the execution test (its
	// content is asserted in TestTranslateInitShape) and run only
	// the file-writing half.
	if idx := strings.Index(script, "# --- runcmd"); idx >= 0 {
		script = script[:idx]
	}
	cmd := exec.Command("bash", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc/kubernetes/pki/ca.crt"))
	if err != nil {
		t.Fatalf("reading written cert: %v", err)
	}
	want := "-----BEGIN CERTIFICATE-----\nFAKE-CERT-FOR-TESTS\n-----END CERTIFICATE-----\n"
	if string(got) != want {
		t.Errorf("cert content = %q, want %q", got, want)
	}
	info, err := os.Stat(filepath.Join(root, "run/kubeadm/kubeadm.yaml"))
	if err != nil {
		t.Fatalf("kubeadm config missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("kubeadm config permissions = %o, want 640", perm)
	}
}

func TestTranslateBase64AndGzipFiles(t *testing.T) {
	raw := []byte("binary-ish content \x00\x01\x02 for a user-supplied file")
	b64 := base64.StdEncoding.EncodeToString(raw)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	gzB64 := base64.StdEncoding.EncodeToString(gz.Bytes())

	root := t.TempDir()
	doc := `## template: jinja
#cloud-config
write_files:
-   path: ` + root + `/plain.bin
    encoding: base64
    content: ` + b64 + `
-   path: ` + root + `/zipped.bin
    encoding: gzip+base64
    content: ` + gzB64 + `
`
	script, err := Translate([]byte(doc))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	cmd := exec.Command("bash", "-c", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated script failed: %v\n%s", err, out)
	}
	for _, name := range []string{"plain.bin", "zipped.bin"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, raw) {
			t.Errorf("%s content mismatch: got %q want %q", name, got, raw)
		}
	}
}

// TestTranslatePermissionsForms covers the filePermissions type:
// CABPK's quoted octal string passes through, and an unquoted 0640
// (which YAML parses as an integer) is recovered as the octal mode
// instead of failing the unmarshal.
func TestTranslatePermissionsForms(t *testing.T) {
	doc := `## template: jinja
#cloud-config
write_files:
-   path: /tmp/quoted
    permissions: '0600'
    content: |
      a
-   path: /tmp/unquoted
    permissions: 0640
    content: |
      b
`
	script, err := Translate([]byte(doc))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.Contains(script, "chmod '0600' '/tmp/quoted'") {
		t.Errorf("quoted permissions not preserved:\n%s", script)
	}
	if !strings.Contains(script, "chmod '0640' '/tmp/unquoted'") {
		t.Errorf("unquoted octal permissions not recovered as 0640:\n%s", script)
	}
}

func TestTranslateRefusesDiskSetup(t *testing.T) {
	doc := `## template: jinja
#cloud-config
write_files: []
disk_setup:
  /dev/sdb:
    table_type: gpt
    layout: true
`
	if _, err := Translate([]byte(doc)); err == nil {
		t.Error("Translate accepted disk_setup; want a refusal error")
	}
}

func TestTranslateRejectsNonCloudConfig(t *testing.T) {
	if _, err := Translate([]byte("#!/bin/bash\necho hi\n")); err == nil {
		t.Error("Translate accepted a non-cloud-config document")
	}
}
