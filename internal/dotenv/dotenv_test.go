package dotenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(path, []byte("# comment\nDOTENV_A=first\nDOTENV_A=last\nexport DOTENV_B=\"quoted\"\nDOTENV_C=from-file\n"), 0o600)
	t.Setenv("DOTENV_A", "")
	t.Setenv("DOTENV_B", "")
	t.Setenv("DOTENV_C", "from-env")
	Load(path)
	for k, want := range map[string]string{"DOTENV_A": "last", "DOTENV_B": "quoted", "DOTENV_C": "from-env"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
