package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInstalledSkills_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	skills, err := LoadInstalledSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skills != nil {
		t.Errorf("LoadInstalledSkills(empty dir) = %v; want nil", skills)
	}
}

func TestLoadInstalledSkills_NoDataDir(t *testing.T) {
	skills, err := LoadInstalledSkills("")
	if err != nil {
		t.Fatal(err)
	}
	if skills != nil {
		t.Errorf("LoadInstalledSkills(\"\") = %v; want nil", skills)
	}
}

func TestSaveAndLoadInstalledSkills(t *testing.T) {
	dir := t.TempDir()
	want := []SkillEntry{
		{Name: "skill-a", GitHub: "org/a", Ref: "main"},
		{Name: "skill-b", GitHub: "org/b", Ref: "v1"},
	}
	if err := SaveInstalledSkills(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadInstalledSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].GitHub != want[i].GitHub || got[i].Ref != want[i].Ref {
			t.Errorf("skill[%d] = %+v; want %+v", i, got[i], want[i])
		}
	}
}

func TestAppendInstalledSkill(t *testing.T) {
	dir := t.TempDir()
	if err := AppendInstalledSkill(dir, SkillEntry{Name: "first", GitHub: "org/first", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendInstalledSkill(dir, SkillEntry{Name: "second", GitHub: "org/second", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	skills, err := LoadInstalledSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("len = %d; want 2", len(skills))
	}
	// Append same name again: idempotent
	if err := AppendInstalledSkill(dir, SkillEntry{Name: "first", GitHub: "org/first", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	skills, _ = LoadInstalledSkills(dir)
	if len(skills) != 2 {
		t.Errorf("after duplicate append len = %d; want 2", len(skills))
	}
}

func TestSaveInstalledSkills_RequiresDataDir(t *testing.T) {
	err := SaveInstalledSkills("", []SkillEntry{{Name: "x", GitHub: "org/x", Ref: "main"}})
	if err == nil {
		t.Error("SaveInstalledSkills(\"\") expected error")
	}
}

func TestLoadInstalledSkills_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, installedSkillsFilename)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not: valid: yaml: here"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInstalledSkills(dir)
	if err == nil {
		t.Error("LoadInstalledSkills(invalid yaml) expected error")
	}
}
