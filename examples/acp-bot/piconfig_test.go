package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// set Pi dir for testing by overriding PI_CODING_AGENT_DIR.
func withTestPiDir(t *testing.T, fn func(piDir string)) {
	t.Helper()
	piDir := t.TempDir()
	old, had := os.LookupEnv("PI_CODING_AGENT_DIR")
	if err := os.Setenv("PI_CODING_AGENT_DIR", piDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("PI_CODING_AGENT_DIR", old)
		} else {
			_ = os.Unsetenv("PI_CODING_AGENT_DIR")
		}
	})
	fn(piDir)
}

func TestMergePiTrustCreatesFile(t *testing.T) {
	withTestPiDir(t, func(piDir string) {
		workDir := t.TempDir()
		if err := mergePiTrust(workDir); err != nil {
			t.Fatalf("mergePiTrust: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(piDir, "trust.json"))
		if err != nil {
			t.Fatalf("trust.json 未创建: %v", err)
		}
		var m map[string]bool
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if !m[workDir] {
			t.Errorf("workDir 未登记: %v", m)
		}
		info, _ := os.Stat(filepath.Join(piDir, "trust.json"))
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("权限应为 0600, 实际 %o", perm)
		}
	})
}

func TestMergePiTrustPreservesOtherEntries(t *testing.T) {
	withTestPiDir(t, func(piDir string) {
		projA := filepath.Join(t.TempDir(), "projA")
		existing := `{"` + projA + `": true, "/tmp/other": false}`
		if err := os.WriteFile(filepath.Join(piDir, "trust.json"), []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
		workDir := t.TempDir()
		if err := mergePiTrust(workDir); err != nil {
			t.Fatalf("mergePiTrust: %v", err)
		}
		data, _ := os.ReadFile(filepath.Join(piDir, "trust.json"))
		var m map[string]bool
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		// 存在性断言（值可为 false——决策双向，不能丢条目）
		for _, key := range []string{workDir, projA, "/tmp/other"} {
			if _, ok := m[key]; !ok {
				t.Errorf("既有决策丢失: %q in %v", key, m)
			}
		}
		if !m[workDir] || !m[projA] {
			t.Errorf("true 决策不应翻转: %v", m)
		}
	})
}

func TestMergePiTrustIdempotent(t *testing.T) {
	withTestPiDir(t, func(piDir string) {
		workDir := t.TempDir()
		if err := mergePiTrust(workDir); err != nil {
			t.Fatal(err)
		}
		if err := mergePiTrust(workDir); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(piDir, "trust.json"))
		var m map[string]bool
		_ = json.Unmarshal(data, &m)
		if !m[workDir] {
			t.Errorf("重复登记后应仍在: %v", m)
		}
		if strings.Count(string(data), workDir) < 1 {
			t.Errorf("内容异常: %s", data)
		}
	})
}

func TestMergePiTrustFailsOnCorruptFile(t *testing.T) {
	withTestPiDir(t, func(piDir string) {
		if err := os.WriteFile(filepath.Join(piDir, "trust.json"), []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := mergePiTrust(t.TempDir()); err == nil {
			t.Error("畸形 trust.json 应报错而不是覆盖")
		}
	})
}
