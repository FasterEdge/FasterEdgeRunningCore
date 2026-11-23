package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileIsEmptyState(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if len(s.Components) != 0 {
		t.Fatalf("应为空状态，实际 %d 条", len(s.Components))
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := Path(dir)
	s := New("linux", "arm64", "edge-01", dir)
	s.Put(Record{Name: "simple-web-shell", Kind: "service", Version: "v1.0.20261124",
		Path: "/opt/ferc/bin/simple-web-shell", Ports: []int{8878}})
	if err := s.Save(p); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	rec, ok := got.Get("simple-web-shell")
	if !ok {
		t.Fatal("应能取回组件")
	}
	if rec.Version != "v1.0.20261124" {
		t.Fatalf("Version = %q", rec.Version)
	}
	if rec.InstalledAt.IsZero() {
		t.Fatal("InstalledAt 应被自动填上")
	}
	if got.GOOS != "linux" || got.Arch != "arm64" || got.Hostname != "edge-01" {
		t.Fatalf("元信息丢失: %+v", got)
	}
}

func TestSaveIsAtomicNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	p := Path(dir)
	s := New("linux", "amd64", "h", dir)
	if err := s.Save(p); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferc-state-") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
}

func TestSaveSorted(t *testing.T) {
	dir := t.TempDir()
	p := Path(dir)
	s := New("linux", "amd64", "h", dir)
	s.Put(Record{Name: "zebra"})
	s.Put(Record{Name: "alpha"})
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var parsed State
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	names := parsed.NamesSorted()
	if len(names) != 2 || names[0] != "alpha" {
		t.Fatalf("保存后应按名字排序，实际 %v", names)
	}
}

func TestPutReplaces(t *testing.T) {
	s := New("linux", "amd64", "h", "/tmp")
	s.Put(Record{Name: "a", Version: "1"})
	s.Put(Record{Name: "a", Version: "2"})
	if len(s.Components) != 1 {
		t.Fatalf("同名记录应被替换，实际 %d 条", len(s.Components))
	}
	rec, _ := s.Get("a")
	if rec.Version != "2" {
		t.Fatalf("Version = %q", rec.Version)
	}
}

func TestPutKeepsInstalledAt(t *testing.T) {
	s := New("linux", "amd64", "h", "/tmp")
	ts := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	s.Put(Record{Name: "a", InstalledAt: ts})
	rec, _ := s.Get("a")
	if !rec.InstalledAt.Equal(ts) {
		t.Fatalf("显式给出的 InstalledAt 应保留，实际 %v", rec.InstalledAt)
	}
}

func TestRemove(t *testing.T) {
	s := New("linux", "amd64", "h", "/tmp")
	s.Put(Record{Name: "a"})
	if !s.Remove("a") {
		t.Fatal("删除已存在的组件应返回 true")
	}
	if s.Remove("a") {
		t.Fatal("删除不存在的组件应返回 false")
	}
}

func TestNames(t *testing.T) {
	s := New("linux", "amd64", "h", "/tmp")
	s.Put(Record{Name: "a"})
	s.Put(Record{Name: "b"})
	m := s.Names()
	if !m["a"] || !m["b"] || len(m) != 2 {
		t.Fatalf("Names = %v", m)
	}
}

func TestLoadRejectsFutureVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte(`{"schema_version":99,"components":[]}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("更高版本的状障文件必须明确报错，而不是按新格式误读")
	}
}

func TestLoadRejectsBrokenJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
}
