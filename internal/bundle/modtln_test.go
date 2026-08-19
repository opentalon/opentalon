package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteModTln(t *testing.T) {
	dir := t.TempDir()
	plugins := []BundlePlugin{
		{Name: "asp", GitHub: "opentalon/tln-asp", Ref: "master"},
		{Name: "db", GitHub: "opentalon/tln-db", Ref: "v0.2.0", Store: true},
	}
	if err := writeModTln(dir, plugins); err != nil {
		t.Fatalf("writeModTln: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "mod.tln"))
	if err != nil {
		t.Fatalf("read mod.tln: %v", err)
	}
	out := string(got)

	wantAsp := `plugin "asp" "master" from "github.com/opentalon/tln-asp"`
	if !strings.Contains(out, wantAsp) {
		t.Errorf("mod.tln missing tool line %q\n%s", wantAsp, out)
	}
	wantDB := `plugin "db" "v0.2.0" store from "github.com/opentalon/tln-db"`
	if !strings.Contains(out, wantDB) {
		t.Errorf("mod.tln missing store line %q\n%s", wantDB, out)
	}
}
